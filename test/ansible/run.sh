#!/usr/bin/env bash
# The Ansible collection's end-to-end test (decision 050): kabsdk.holdfast built and installed as people install it, its
# playbook kabsdk.holdfast.site run on machines with systemd (containers, reached with the docker connection), against
# a real S3 server (MinIO), with the packages `make packages` built in dist/ served over HTTP as a mirror.
#   - ansible-lint passes with the production profile;
#   1. the least settings, server on AlmaLinux, agents on Ubuntu and Oracle Linux: the server, its recovery file and UI;
#      each agent added, enrolled and given its jobs, one of them a dump defined in agent.yaml; backups through the
#      gateway; run again, nothing changes; a setting changed reaches server.yaml; the server lost and replaced from the
#      recovery file alone, the agents back by themselves; run again, nothing changes;
#   2. your own age key, UI certificate, OIDC, mail and off-site copies, server on Ubuntu, an agent on AlmaLinux enrolled
#      with a code given as the installer takes it: run again, nothing changes; enrolled again on purpose.
# Usage: test/ansible/run.sh     (make ansible-test). KEEP=1 leaves the containers running for a look.
set -euo pipefail
cd "$(dirname "$0")/../.."

MINIO_IMAGE=${MINIO_IMAGE:?} MC_IMAGE=${MC_IMAGE:?} HOOK_IMAGE=${HOOK_IMAGE:-alpine:latest} MIRROR_IMAGE=${MIRROR_IMAGE:-busybox:latest}
[ -s dist/VERSION ] && [ -s dist/SHA256SUMS ] || { echo "dist/ has no release: make packages first" >&2; exit 1; }

net=hf-ansible
work=$(mktemp -d)
chmod 0755 "$work" # mc's image runs as a user of its own; what is here is the test's own throwaway material
containers=()
cleanup() {
	if [ "${KEEP:-}" = 1 ]; then echo "KEEP=1: containers ${containers[*]} and $work are left"; return; fi
	docker rm -f "${containers[@]}" hf-ansible-minio hf-ansible-hook hf-ansible-mirror >/dev/null 2>&1 || true
	docker network rm $net >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT
pass=0
ok() { pass=$((pass + 1)); echo "PASS $*"; }
fail() { echo "FAIL $*" >&2; exit 1; }

# A machine with systemd, Python for Ansible and sudo for become, and nothing of Holdfast's.
image_for() {
	local base=$1 tag
	tag=holdfast-ansible-test:$(echo "$base" | tr ':/' '--')
	case "$base" in
	ubuntu* | debian*) printf 'FROM %s\nRUN apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -y -q systemd systemd-sysv python3 sudo curl ca-certificates && apt-get clean && rm -f /etc/dpkg/dpkg.cfg.d/excludes\nSTOPSIGNAL SIGRTMIN+3\nCMD ["/sbin/init"]\n' "$base" ;;
	*) printf 'FROM %s\nRUN dnf install -y -q systemd procps-ng hostname python3 sudo && dnf clean all\nSTOPSIGNAL SIGRTMIN+3\nCMD ["/sbin/init"]\n' "$base" ;;
	esac | docker build -q -t "$tag" - >/dev/null
	echo "$tag"
}
machine() { # machine NAME IMAGE: a container with systemd as PID 1, on the test network
	docker run -d --name "$1" --hostname "$1" --network $net --network-alias "$1" --privileged --cgroupns=private \
		--tmpfs /run --tmpfs /run/lock --tmpfs /tmp "$2" >/dev/null
	containers+=("$1")
	for _ in $(seq 60); do
		case "$(docker exec "$1" systemctl is-system-running 2>/dev/null || true)" in running | degraded) return 0 ;; esac
		sleep 1
	done
	fail "$1: systemd did not come up"
}
mcx() { docker run --rm --network $net --entrypoint /usr/bin/mc -e MC_HOST_hf="http://hfroot:hfroot-ansible-secret@hf-ansible-minio:9000" -v "$work:/work:ro" "$MC_IMAGE" "$@"; }
backups() { { docker exec "$1" holdfast host events "$2" --json | grep -o '"status": *"success"' || true; } | wc -l; }
wait_backup() { # wait_backup SERVER FQDN BEFORE: until the host has more successful backups than BEFORE
	local now=0
	for _ in $(seq 90); do
		now=$(backups "$1" "$2")
		[ "$now" -gt "$3" ] && return 0
		sleep 2
	done
	fail "$2: no backup: $(docker exec "$1" holdfast host events "$2")"
}
bucket() { # bucket NAME USER-PREFIX: a versioned bucket with a lifecycle rule, and its gateway and maintenance keys
	local b=$1 k=$2
	mcx mb --quiet "hf/$b" >/dev/null && mcx version enable "hf/$b" >/dev/null
	mcx ilm rule add --noncurrent-expire-days 30 "hf/$b" >/dev/null
	cat >"$work/gw-$k.json" <<EOF
{"Version": "2012-10-17", "Statement": [
  {"Effect": "Allow", "Action": ["s3:ListBucket", "s3:GetBucketLocation"], "Resource": ["arn:aws:s3:::$b"]},
  {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject"], "Resource": ["arn:aws:s3:::$b/repos/*"]},
  {"Effect": "Allow", "Action": ["s3:DeleteObject"], "Resource": ["arn:aws:s3:::$b/repos/*/locks/*"]},
  {"Effect": "Deny", "Action": ["s3:DeleteObjectVersion"], "Resource": ["arn:aws:s3:::$b/*"]}]}
EOF
	cat >"$work/maint-$k.json" <<EOF
{"Version": "2012-10-17", "Statement": [
  {"Effect": "Allow", "Action": ["s3:ListBucket", "s3:ListBucketVersions", "s3:GetBucketLocation", "s3:GetBucketVersioning", "s3:GetLifecycleConfiguration"], "Resource": ["arn:aws:s3:::$b"]},
  {"Effect": "Allow", "Action": ["s3:GetObject", "s3:GetObjectVersion", "s3:PutObject", "s3:DeleteObject"], "Resource": ["arn:aws:s3:::$b/repos/*"]},
  {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"], "Resource": ["arn:aws:s3:::$b/escrow/*"]},
  {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"], "Resource": ["arn:aws:s3:::$b/meta/*"]},
  {"Effect": "Deny", "Action": ["s3:DeleteObjectVersion"], "Resource": ["arn:aws:s3:::$b/*"]}]}
EOF
	for p in gw maint; do
		mcx admin user add hf "$p$k" "$p-$k-ansible-secret" >/dev/null
		mcx admin policy create hf "$p-$k" "/work/$p-$k.json" >/dev/null
		mcx admin policy attach hf "$p-$k" --user "$p$k" >/dev/null
	done
}
# play NAME INVENTORY [ARGS…]: kabsdk.holdfast.site, its output in $work/NAME.log
play() {
	local name=$1 inv=$2
	shift 2
	ansible-playbook -i "$inv" kabsdk.holdfast.site "$@" >"$work/$name.log" 2>&1 || {
		grep -B3 -A12 -E '^(fatal|failed):' "$work/$name.log" | head -80 >&2
		sed -n '/^PLAY RECAP/,$p' "$work/$name.log" >&2
		fail "$name: the playbook failed (above)"
	}
}
unchanged() { # unchanged NAME: the recap of $work/NAME.log has changed=0 for every host
	local recap
	recap=$(sed -n '/^PLAY RECAP/,$p' "$work/$1.log")
	grep -qE ': ok=[0-9]+ +changed=' <<<"$recap" || fail "$1: no recap"
	if grep -E ': ok=' <<<"$recap" | grep -vE 'changed=0 '; then
		grep -E '^(changed|TASK)' "$work/$1.log" | grep -B1 '^changed' >&2 || true
		fail "$1: a run with nothing to do changed something (above)"
	fi
}

# The collection, built and installed as people get it; its dependencies are the ones installed here.
ansible-galaxy collection build ansible --output-path "$work" >/dev/null
ansible-galaxy collection install --no-deps -p "$work/collections" "$work"/kabsdk-holdfast-*.tar.gz >/dev/null
export ANSIBLE_COLLECTIONS_PATH="$work/collections:$HOME/.ansible/collections:/usr/share/ansible/collections"
export ANSIBLE_NOCOLOR=1 ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_HOST_KEY_CHECKING=False ANSIBLE_RETRY_FILES_ENABLED=False
(cd "$work/collections/ansible_collections/kabsdk/holdfast" && ansible-lint --nocolor -q >"$work/lint.log" 2>&1) || {
	cat "$work/lint.log" >&2
	fail "ansible-lint"
}
ok "the collection builds, installs, and passes ansible-lint's production profile"

docker network create $net >/dev/null
docker run -d --name hf-ansible-minio --network $net -e MINIO_ROOT_USER=hfroot -e MINIO_ROOT_PASSWORD=hfroot-ansible-secret "$MINIO_IMAGE" server /data >/dev/null
for _ in $(seq 30); do mcx ready hf >/dev/null 2>&1 && break; sleep 1; done
# The packages, served as a mirror of one release is (holdfast_download_url).
docker run -d --name hf-ansible-mirror --network $net -v "$PWD/dist:/dist:ro" "$MIRROR_IMAGE" httpd -f -p 8080 -h /dist >/dev/null
# The alert receiver: it prints each request it gets, for docker logs.
CGO_ENABLED=0 GOTOOLCHAIN=local go build -o "$work/hook" ./test/install/hook
docker run -d --name hf-ansible-hook --network $net -v "$work/hook:/hook:ro" "$HOOK_IMAGE" /hook >/dev/null
mirror=http://hf-ansible-mirror:8080

# ------------------------------------------------------------------------------------------------ 1: the least settings
srv=hfsrv1.ansible.test a1=agent1-1.ansible.test a2=agent1-2.ansible.test
bucket holdfast-1 1
machine $srv "$(image_for almalinux:9)"
machine $a1 "$(image_for ubuntu:24.04)"
machine $a2 "$(image_for oraclelinux:9)"
inventory() { # inventory WEBHOOK-PATH
	cat <<EOF
all:
  vars:
    ansible_connection: community.docker.docker
    ansible_python_interpreter: /usr/bin/python3
    holdfast_download_url: $mirror
  children:
    holdfast_server:
      hosts:
        $srv:
          holdfast_s3_endpoint: http://hf-ansible-minio:9000
          holdfast_s3_bucket: holdfast-1
          holdfast_gateway_access_key_id: gw1
          holdfast_gateway_secret_access_key: gw-1-ansible-secret
          holdfast_maintenance_access_key_id: maint1
          holdfast_maintenance_secret_access_key: maint-1-ansible-secret
          holdfast_webhook_url: http://hf-ansible-hook:8080/$1
          holdfast_heartbeat_url: http://hf-ansible-hook:8080/beat-1
    holdfast_agents:
      hosts:
        $a1:
          holdfast_dumps:
            hostname: {command: [/usr/bin/cat, /etc/hostname]}
          holdfast_jobs:
            etc: --path /etc
            hostname: --dump hostname --every 12h
        $a2:
          holdfast_jobs:
            etc: --path /etc --path /usr/share/doc --daily 03:00
    holdfast_cli:
      hosts:
        $a1:
EOF
}
inventory alert-1 >"$work/inv-1.yml"
play first-1 "$work/inv-1.yml"
docker exec $srv holdfast status >/dev/null || fail "holdfast status after the playbook"
docker exec $srv sh -c 'stat -c "%a %n" /etc/holdfast /etc/holdfast/tls /etc/holdfast/*.env /etc/holdfast/db.dsn /etc/holdfast/credentials.key /etc/holdfast/tls/*.key /etc/holdfast/server.yaml /root/holdfast-recovery.txt' >"$work/modes" ||
	fail "the server's files"
if grep -vE '^(700|600) ' "$work/modes"; then fail "a secret is readable by others"; fi
if docker exec $srv grep -lE 'ansible-secret' /etc/holdfast/server.yaml; then fail "a secret is in server.yaml"; fi
docker exec $srv grep -q '^AGE-SECRET-KEY-' /root/holdfast-recovery.txt || fail "the recovery file holds no age key"
docker exec $srv grep -q '^HOLDFAST-CREDENTIALS-KEY-' /root/holdfast-recovery.txt || fail "the recovery file holds no credentials key"
grep -q "The recovery file is /root/holdfast-recovery.txt on this server" "$work/first-1.log" || fail "the summary does not name the recovery file"
grep -q "root logs in with the link holdfast ui-login prints" "$work/first-1.log" || fail "the summary does not say how to log in"
pin=$(sed -n 's/^ca_sha256: *//p' <(docker exec $srv cat /etc/holdfast/server.yaml) | tr -d "\"'")
[[ $pin =~ ^[0-9a-f]{64}$ ]] || fail "server.yaml has no pin: $pin"
code=$(docker exec $srv curl -sk -o /dev/null -w '%{http_code}' https://localhost/)
[[ $code =~ ^(200|30[23])$ ]] || fail "the UI answered $code"
ok "almalinux:9: the server, with PostgreSQL, its CA ($pin), the recovery file, keys root-only, the UI on :443"

for h in $a1 $a2; do
	show=$(docker exec $srv holdfast host show $h --json)
	grep -q '"state": *"enrolled"' <<<"$show" || fail "$h is not enrolled: $show"
	docker exec $srv holdfast job run $h etc >/dev/null || fail "$h: job run"
	wait_backup $srv $h 0
done
docker exec $srv holdfast host show $a1 --json | grep -q '"id": *"hostname"' || fail "$a1 has no dump job"
before=$(backups $srv $a1)
docker exec $srv holdfast job run $a1 hostname >/dev/null || fail "$a1: the dump job's run"
wait_backup $srv $a1 "$before"
docker exec $a1 holdfast --help >/dev/null || fail "the CLI does not run on $a1"
ok "ubuntu:24.04, oraclelinux:9: each agent added, enrolled, given its jobs and a dump; backups go through the gateway; the CLI"

play again-1 "$work/inv-1.yml"
unchanged again-1
ok "run again, nothing changes"

out=$(docker exec $srv holdfast notify test 2>&1) || fail "notify test: $out"
for _ in $(seq 10); do docker logs hf-ansible-hook 2>&1 | grep -q '^POST /alert-1 .*"text"' && break; sleep 1; done
docker logs hf-ansible-hook 2>&1 | grep -q '^POST /alert-1 .*"text"' || fail "the test alert did not reach the webhook: $out"
inventory alert-1b >"$work/inv-1.yml"
play change-1 "$work/inv-1.yml"
grep -q 'RUNNING HANDLER \[kabsdk.holdfast.server : Restart holdfast-server\]' "$work/change-1.log" || fail "a changed setting did not restart the server"
docker exec $srv holdfast notify test >/dev/null 2>&1 || fail "notify test after the change"
for _ in $(seq 10); do docker logs hf-ansible-hook 2>&1 | grep -q '^POST /alert-1b ' && break; sleep 1; done
docker logs hf-ansible-hook 2>&1 | grep -q '^POST /alert-1b ' || fail "the changed webhook does not get the alerts"
ok "a changed setting reaches server.yaml, and the server restarts on it"

# The server lost and replaced on a new machine, from the recovery file and the same inventory.
docker exec $srv cat /root/holdfast-recovery.txt >"$work/recovery-1.txt"
docker exec $srv rm /root/holdfast-recovery.txt
docker exec $srv holdfast export now >/dev/null || fail "export now"
before=$(backups $srv $a2)
docker rm -f $srv >/dev/null
machine $srv "$(image_for almalinux:9)"
play rebuild-1 "$work/inv-1.yml" -e "holdfast_recovery_file=$work/recovery-1.txt"
grep -q 'TASK \[kabsdk.holdfast.server : Restore it (restore-export -latest)\]' "$work/rebuild-1.log" || fail "the rebuild did not restore the export"
[ "$(sed -n 's/^ca_sha256: *//p' <(docker exec $srv cat /etc/holdfast/server.yaml) | tr -d "\"'")" = "$pin" ] || fail "the rebuilt server has another pin"
for _ in $(seq 60); do docker exec $srv holdfast host show $a2 --json 2>/dev/null | grep -q '"online": *true' && break; sleep 2; done
docker exec $srv holdfast job run $a2 etc >/dev/null || fail "rebuilt: job run"
wait_backup $srv $a2 "$before"
ok "the server rebuilt from the recovery file and the same inventory, with the same pin; the agents back by themselves"

docker exec $srv rm /root/holdfast-recovery.txt
play again-1b "$work/inv-1.yml" -e "holdfast_recovery_file=$work/recovery-1.txt"
unchanged again-1b
ok "run again after the rebuild, the recovery file deleted on the server and still in the inventory: nothing changes"
docker rm -f $srv $a1 $a2 >/dev/null

# ---------------------------------------------------------------------- 2: your own age key, certificates, OIDC, off-site
srv=hfsrv2.ansible.test a1=agent2-1.ansible.test
bucket holdfast-2 2
bucket holdfast-2-offsite 2o
GOTOOLCHAIN=local go run ./lab/escrowkey "$work/escrow-key.txt" >/dev/null
recipient=$(sed -n 's/^# public key: //p' "$work/escrow-key.txt")
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=$srv -days 2 -keyout "$work/ui.key" -out "$work/ui.crt" 2>/dev/null
machine $srv "$(image_for ubuntu:24.04)"
machine $a1 "$(image_for almalinux:9)"
server_vars() {
	cat <<EOF
          holdfast_s3_endpoint: http://hf-ansible-minio:9000
          holdfast_s3_bucket: holdfast-2
          holdfast_gateway_access_key_id: gw2
          holdfast_gateway_secret_access_key: gw-2-ansible-secret
          holdfast_maintenance_access_key_id: maint2
          holdfast_maintenance_secret_access_key: maint-2-ansible-secret
          holdfast_escrow_recipient: $recipient
          holdfast_zone: Europe/Copenhagen
          holdfast_ui_url: https://$srv:9443
          holdfast_ui_tls_cert: $work/ui.crt
          holdfast_ui_tls_key: $work/ui.key
          holdfast_oidc_issuer: https://auth.ansible.test
          holdfast_oidc_client_id: holdfast
          holdfast_oidc_client_secret: oidc-2-ansible-secret
          holdfast_admins: [alice]
          holdfast_mail_to: [backups@ansible.test]
          holdfast_smtp: relay.ansible.test:587
          holdfast_smtp_user: holdfast
          holdfast_smtp_password: smtp-2-ansible-secret
          holdfast_offsite_endpoint: http://hf-ansible-minio:9000
          holdfast_offsite_bucket: holdfast-2-offsite
          holdfast_offsite_access_key_id: maint2o
          holdfast_offsite_secret_access_key: maint-2o-ansible-secret
          holdfast_server_yaml: {notify: {after: 30m}}
EOF
}
# First the server alone; the agent then enrolls with a code given as the installer takes it.
cat >"$work/inv-2.yml" <<EOF
all:
  vars:
    ansible_connection: community.docker.docker
    ansible_python_interpreter: /usr/bin/python3
    holdfast_download_url: $mirror
  children:
    holdfast_server:
      hosts:
        $srv:
$(server_vars)
EOF
play first-2 "$work/inv-2.yml"
docker exec $srv grep -q 'after: 30m' /etc/holdfast/server.yaml || fail "holdfast_server_yaml is not in server.yaml"
docker exec $srv grep -q 'default_zone: Europe/Copenhagen' /etc/holdfast/server.yaml || fail "the zone is not in server.yaml"
docker exec $srv grep -q '^HOLDFAST-CREDENTIALS-KEY-' /root/holdfast-recovery.txt || fail "the recovery file holds no credentials key"
if docker exec $srv grep -q '^AGE-SECRET-KEY-' /root/holdfast-recovery.txt; then fail "the recovery file holds an age key, with your own"; fi
grep -q "Add the AGE-SECRET-KEY-… line of your own age key to it" "$work/first-2.log" || fail "the summary does not say to add the age key"
for f in oidc-client-secret smtp-password offsite.env; do
	[ "$(docker exec $srv stat -c %a /etc/holdfast/$f)" = 600 ] || fail "/etc/holdfast/$f is not root-only"
done
if docker exec $srv grep -lE 'ansible-secret' /etc/holdfast/server.yaml; then fail "a secret is in server.yaml"; fi
[ "$(docker exec $srv curl -sk -o /dev/null -w '%{http_code}' https://localhost:9443/api/v1/me)" = 401 ] || fail "the UI on :9443"
docker exec $srv openssl x509 -noout -subject -in /etc/holdfast/tls/ui.crt | grep -q "CN *= *$srv" || fail "the UI's certificate is not yours"
ok "ubuntu:24.04: the server with your own age key, UI certificate, OIDC, mail, off-site copies and server.yaml additions"

add=$(docker exec $srv holdfast host add $a1 --json) || fail "host add: $add"
line=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["enrollment"]["install_script"])' <<<"$add")
code=$(sed -nE 's/.*HOLDFAST_CODE=([^ ]+).*/\1/p' <<<"$line" | tr -d "'")
server=$(sed -nE 's/.*HOLDFAST_SERVER=([^ ]+).*/\1/p' <<<"$line" | tr -d "'")
capin=$(sed -nE 's/.*HOLDFAST_CA_SHA256=([^ ]+).*/\1/p' <<<"$line" | tr -d "'")
cat >>"$work/inv-2.yml" <<EOF
    holdfast_agents:
      hosts:
        $a1:
          holdfast_server: $server
          holdfast_code: $code
          holdfast_ca_sha256: $capin
          holdfast_restore: {allow_in_place: true}
EOF
play agent-2 "$work/inv-2.yml"
docker exec $a1 grep -q 'allow_in_place: true' /etc/holdfast/agent.yaml || fail "the restore bounds are not in agent.yaml"
docker exec $srv holdfast job set $a1 etc --path /etc >/dev/null || fail "job set"
docker exec $srv holdfast job run $a1 etc >/dev/null || fail "job run"
wait_backup $srv $a1 0
ok "almalinux:9: the agent enrolled with a code given as the installer takes it, its restore bounds set, and backing up"

play again-2 "$work/inv-2.yml"
unchanged again-2
ok "run again, with the used code still in the inventory: nothing changes"

play reenroll-2 "$work/inv-2.yml" -e holdfast_reenroll=true -e holdfast_code=
grep -q 'TASK \[kabsdk.holdfast.agent : Get a new code for the host Holdfast knows' "$work/reenroll-2.log" || fail "the enrollment again did not get a code"
before=$(backups $srv $a1)
docker exec $srv holdfast job run $a1 etc >/dev/null || fail "job run after enrolling again"
wait_backup $srv $a1 "$before"
ok "holdfast_reenroll enrolls the agent again, with a new code from the server, and it backs up"

echo "ansible test: $pass passed"
