#!/usr/bin/env bash
# The installer's end-to-end test (decisions 044, plan-m10): dist/install.sh run as a person runs it, `curl … | sh`, on
# machines with systemd, against a real S3 server (MinIO), with the packages `make packages` built in dist/. Each
# scenario is a server and one or more agents, each a container of its own on one network:
#   - the installer refuses a server with settings missing, and changes nothing;
#   - the server installs with its own PostgreSQL, CA, the recovery file and the web UI, and starts. The odd scenarios
#     take the least there is: the installer makes the age key, root logs in to the UI with holdfast ui-login, and
#     alerts go to a webhook, with a heartbeat. The even ones give their own age key, UI certificate and OIDC;
#   - `holdfast host add` prints the one line; on each agent it installs, enrolls, and a backup goes through the gateway;
#   - run again, it only upgrades, on the server and on an agent;
#   - the CLI installs;
#   - the server lost and replaced on a new machine: in the odd scenarios with the recovery file alone, as the docs have
#     it; in the even ones with /etc/holdfast put back and restore-export. Either way the agent comes back by itself and
#     backs up.
# Usage: test/install/run.sh [<server image>:<agent image>[,<agent image>…] …]     (make install-test)
# KEEP=1 leaves the containers running for a look.
set -euo pipefail
cd "$(dirname "$0")/../.."

MINIO_IMAGE=${MINIO_IMAGE:?} MC_IMAGE=${MC_IMAGE:?} HOOK_IMAGE=${HOOK_IMAGE:-alpine:latest}
scenarios=("$@")
[ ${#scenarios[@]} -gt 0 ] || scenarios=("almalinux:9:ubuntu:24.04,oraclelinux:9" "ubuntu:24.04:almalinux:9")
[ -s dist/install.sh ] && [ -s dist/VERSION ] || { echo "dist/ has no release: make packages first" >&2; exit 1; }

net=hf-install
work=$(mktemp -d)
chmod 0755 "$work" # mc's image runs as a user of its own; what is here is the test's own throwaway material
containers=()
cleanup() {
	if [ "${KEEP:-}" = 1 ]; then echo "KEEP=1: containers ${containers[*]} and $work are left"; return; fi
	docker rm -f "${containers[@]}" hf-install-minio hf-install-hook >/dev/null 2>&1 || true
	docker network rm $net >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT
pass=0
ok() { pass=$((pass + 1)); echo "PASS $*"; }
fail() { echo "FAIL $*" >&2; exit 1; }

# A machine with systemd and nothing else of Holdfast's: the installer brings the rest.
image_for() {
	local base=$1 tag
	tag=holdfast-install-test:$(echo "$base" | tr ':/' '--')
	case "$base" in
	ubuntu* | debian*) printf 'FROM %s\nRUN apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -y -q systemd systemd-sysv curl ca-certificates && apt-get clean && rm -f /etc/dpkg/dpkg.cfg.d/excludes\nSTOPSIGNAL SIGRTMIN+3\nCMD ["/sbin/init"]\n' "$base" ;;
	*) printf 'FROM %s\nRUN dnf install -y -q systemd procps-ng hostname && dnf clean all\nSTOPSIGNAL SIGRTMIN+3\nCMD ["/sbin/init"]\n' "$base" ;;
	esac | docker build -q -t "$tag" - >/dev/null
	echo "$tag"
}
machine() { # machine NAME IMAGE: a container with systemd as PID 1, on the test network, with dist/ and the work dir
	docker run -d --name "$1" --hostname "$1" --network $net --network-alias "$1" --privileged --cgroupns=private \
		--tmpfs /run --tmpfs /run/lock --tmpfs /tmp -v "$PWD/dist:/dist:ro" -v "$work:/work:ro" "$2" >/dev/null
	containers+=("$1")
	for _ in $(seq 60); do
		case "$(docker exec "$1" systemctl is-system-running 2>/dev/null || true)" in running | degraded) return 0 ;; esac
		sleep 1
	done
	fail "$1: systemd did not come up"
}
mcx() { docker run --rm --network $net --entrypoint /usr/bin/mc -e MC_HOST_hf="http://hfroot:hfroot-install-secret@hf-install-minio:9000" -v "$work:/work:ro" "$MC_IMAGE" "$@"; }
# backups SERVER FQDN: how many successful backups the host has; 0 before the first (grep finds none, which pipefail
# would make an error).
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

docker network create $net >/dev/null
docker run -d --name hf-install-minio --network $net -e MINIO_ROOT_USER=hfroot -e MINIO_ROOT_PASSWORD=hfroot-install-secret "$MINIO_IMAGE" server /data >/dev/null
for _ in $(seq 30); do mcx ready hf >/dev/null 2>&1 && break; sleep 1; done
# The alert receiver: it prints each request it gets, for docker logs.
CGO_ENABLED=0 GOTOOLCHAIN=local go build -o "$work/hook" ./test/install/hook
docker run -d --name hf-install-hook --network $net -v "$work/hook:/hook:ro" "$HOOK_IMAGE" /hook >/dev/null
# The even scenarios' own age key, and the UI's certificate: a self-signed one is enough to see the listener answer.
GOTOOLCHAIN=local go run ./lab/escrowkey "$work/escrow-key.txt" >/dev/null
recipient=$(sed -n 's/^# public key: //p' "$work/escrow-key.txt")
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=hfsrv -days 2 -keyout "$work/ui.key" -out "$work/ui.crt" 2>/dev/null
chmod 0644 "$work/ui.key" # read by root in the containers; it is a throwaway key

n=0
for sc in "${scenarios[@]}"; do
	n=$((n + 1))
	srv_image=$(echo "$sc" | sed -E 's/^([^:]+:[^:]+):.*/\1/')
	agent_images=$(echo "$sc" | sed -E 's/^[^:]+:[^:]+://' | tr ',' ' ')
	bucket=holdfast-$n srv=hfsrv$n.install.test
	least=false
	[ $((n % 2)) = 1 ] && least=true
	echo "== scenario $n: server on $srv_image, agents on $agent_images ($($least && echo 'the least settings' || echo 'own age key, UI certificate and OIDC'))"

	# The bucket and its keys, as docs/storage.md has them.
	mcx mb --quiet hf/$bucket >/dev/null && mcx version enable hf/$bucket >/dev/null
	mcx ilm rule add --noncurrent-expire-days 30 hf/$bucket >/dev/null
	cat >"$work/gw-$n.json" <<EOF
{"Version": "2012-10-17", "Statement": [
  {"Effect": "Allow", "Action": ["s3:ListBucket", "s3:GetBucketLocation"], "Resource": ["arn:aws:s3:::$bucket"]},
  {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject"], "Resource": ["arn:aws:s3:::$bucket/repos/*"]},
  {"Effect": "Allow", "Action": ["s3:DeleteObject"], "Resource": ["arn:aws:s3:::$bucket/repos/*/locks/*"]},
  {"Effect": "Deny", "Action": ["s3:DeleteObjectVersion"], "Resource": ["arn:aws:s3:::$bucket/*"]}]}
EOF
	cat >"$work/maint-$n.json" <<EOF
{"Version": "2012-10-17", "Statement": [
  {"Effect": "Allow", "Action": ["s3:ListBucket", "s3:ListBucketVersions", "s3:GetBucketLocation", "s3:GetBucketVersioning", "s3:GetLifecycleConfiguration"], "Resource": ["arn:aws:s3:::$bucket"]},
  {"Effect": "Allow", "Action": ["s3:GetObject", "s3:GetObjectVersion", "s3:PutObject", "s3:DeleteObject"], "Resource": ["arn:aws:s3:::$bucket/repos/*"]},
  {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"], "Resource": ["arn:aws:s3:::$bucket/escrow/*"]},
  {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"], "Resource": ["arn:aws:s3:::$bucket/meta/*"]},
  {"Effect": "Deny", "Action": ["s3:DeleteObjectVersion"], "Resource": ["arn:aws:s3:::$bucket/*"]}]}
EOF
	for k in gw maint; do
		mcx admin user add hf "$k$n" "$k-$n-install-secret" >/dev/null
		mcx admin policy create hf "$k-$n" "/work/$k-$n.json" >/dev/null
		mcx admin policy attach hf "$k-$n" --user "$k$n" >/dev/null
	done

	# The settings, in a file the admin sources, so the secrets stay out of the shell's history.
	cat >"$work/server-$n.env" <<EOF
HOLDFAST_DOWNLOAD_URL=file:///dist
HOLDFAST_S3_ENDPOINT=http://hf-install-minio:9000
HOLDFAST_S3_BUCKET=$bucket
HOLDFAST_GATEWAY_ACCESS_KEY_ID=gw$n
HOLDFAST_GATEWAY_SECRET_ACCESS_KEY=gw-$n-install-secret
HOLDFAST_MAINTENANCE_ACCESS_KEY_ID=maint$n
HOLDFAST_MAINTENANCE_SECRET_ACCESS_KEY=maint-$n-install-secret
EOF
	if $least; then
		cat >>"$work/server-$n.env" <<EOF
HOLDFAST_WEBHOOK_URL=http://hf-install-hook:8080/alert-$n
HOLDFAST_HEARTBEAT_URL=http://hf-install-hook:8080/beat-$n
EOF
	else
		cat >>"$work/server-$n.env" <<EOF
HOLDFAST_ESCROW_RECIPIENT=$recipient
HOLDFAST_UI_URL=https://$srv
HOLDFAST_UI_TLS_CERT=/work/ui.crt
HOLDFAST_UI_TLS_KEY=/work/ui.key
HOLDFAST_OIDC_ISSUER=https://auth.install.test
HOLDFAST_OIDC_CLIENT_ID=holdfast
HOLDFAST_OIDC_CLIENT_SECRET=oidc-$n-install-secret
HOLDFAST_ADMINS=alice
EOF
	fi

	machine $srv "$(image_for "$srv_image")"

	# Settings missing: refused, every one named, nothing installed.
	if out=$(docker exec $srv sh -c 'curl -sfL file:///dist/install.sh | HOLDFAST_DOWNLOAD_URL=file:///dist HOLDFAST_S3_BUCKET=x HOLDFAST_ESCROW_RECIPIENT=nope sh -s - server' 2>&1); then
		fail "the installer took a server with settings missing: $out"
	fi
	for v in HOLDFAST_S3_ENDPOINT HOLDFAST_GATEWAY_ACCESS_KEY_ID HOLDFAST_MAINTENANCE_SECRET_ACCESS_KEY "HOLDFAST_ESCROW_RECIPIENT: is not an age public key" "Nothing was changed"; do
		grep -qF -- "$v" <<<"$out" || fail "the refusal does not name $v: $out"
	done
	docker exec $srv sh -c '! command -v holdfast-server && ! command -v psql' >/dev/null || fail "the refused install changed the machine"
	ok "$srv_image: a server with settings missing is refused, every one named, nothing changed"

	# The server, as the docs have it.
	out=$(docker exec $srv sh -c "set -a; . /work/server-$n.env; set +a; curl -sfL file:///dist/install.sh | sh -s - server" 2>&1) || fail "server install: $out"
	pin=$(sed -n 's/^  Agents connect to .*, and pin //p' <<<"$out")
	[[ $pin =~ ^[0-9a-f]{64}$ ]] || fail "the server's install printed no pin: $out"
	grep -qF "The recovery file is /root/holdfast-recovery.txt" <<<"$out" || fail "the install does not name the recovery file: $out"
	if $least; then
		grep -q "log in with the link that holdfast ui-login prints" <<<"$out" || fail "the summary does not say how to log in: $out"
		if grep -q "No alerts are set up" <<<"$out"; then fail "the summary says no alerts are set up, with a webhook: $out"; fi
	else
		grep -q "people log in through https://auth.install.test" <<<"$out" || fail "the summary does not name the identity provider: $out"
		grep -q "No alerts are set up" <<<"$out" || fail "the summary does not say that no alerts are set up: $out"
		grep -q "add your own age key's AGE-SECRET-KEY" <<<"$out" || fail "the summary does not say to add the age key: $out"
	fi
	docker exec $srv holdfast status >/dev/null || fail "holdfast status after the install"
	docker exec $srv sh -c 'stat -c "%a %n" /etc/holdfast /etc/holdfast/*.env /etc/holdfast/db.dsn /etc/holdfast/credentials.key /etc/holdfast/tls/*.key /etc/holdfast/server.yaml /root/holdfast-recovery.txt; ! [ -e /etc/holdfast/oidc-client-secret ] || stat -c "%a %n" /etc/holdfast/oidc-client-secret' >"$work/modes" ||
		fail "the server's files"
	if grep -vE '^(700|600) ' "$work/modes"; then fail "a secret is readable by others"; fi
	if docker exec $srv grep -rlE 'install-secret' /etc/holdfast/server.yaml; then fail "a secret is in server.yaml"; fi
	if $least; then
		docker exec $srv grep -q '^AGE-SECRET-KEY-' /root/holdfast-recovery.txt || fail "the recovery file holds no age key"
	fi
	docker exec $srv grep -q '^HOLDFAST-CREDENTIALS-KEY-' /root/holdfast-recovery.txt || fail "the recovery file holds no credentials key"
	docker exec $srv holdfast status --json | grep -q "\"recovery_file\"" || fail "the server does not show recovery_file: $(docker exec $srv holdfast status)"
	code=$(docker exec $srv curl -sk -o /dev/null -w '%{http_code}' https://localhost/)
	[[ $code =~ ^(200|30[23])$ ]] || fail "the UI answered $code"
	ok "$srv_image: the server installs with PostgreSQL, its CA ($pin), the recovery file, keys root-only, the UI on :443"

	# The way into the UI without an identity provider, and in when it is down: a one-time link from root.
	link=$(docker exec $srv holdfast ui-login | sed -nE 's/^  (https:[^ ]+)$/\1/p')
	[[ $link == "https://$srv/auth/link?t="* ]] || fail "ui-login's link: $link"
	me=$(docker exec $srv sh -c "curl -sfk -c /tmp/jar -o /dev/null '$link' && curl -sfk -b /tmp/jar https://$srv/api/v1/me") || fail "the link did not log in"
	grep -q '"admin_everywhere": *true' <<<"$me" || fail "the link's session: $me"
	code=$(docker exec $srv curl -sk -o /dev/null -w '%{http_code}' "$link")
	[ "$code" = 403 ] || fail "the link worked twice: $code"
	ok "holdfast ui-login gives a link that logs in once, as an admin everywhere"

	add=$(docker exec $srv holdfast host add agent$n-1.install.test --json) || fail "host add: $add"
	docker exec $srv holdfast job set agent$n-1.install.test etc --path /etc >/dev/null || fail "job set with the default schedule and retention"
	line=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["enrollment"]["install_script"])' <<<"$add")
	[[ $line == "curl -sfL file:///dist/install.sh | HOLDFAST_SERVER=https://$srv:8443 HOLDFAST_FQDN=agent$n-1.install.test "*" HOLDFAST_CA_SHA256=$pin HOLDFAST_DOWNLOAD_URL=file:///dist sh -" ]] ||
		fail "host add's one line: $line"
	ok "host add prints the one line: $line"

	i=0
	for img in $agent_images; do
		i=$((i + 1))
		fqdn=agent$n-$i.install.test
		if [ $i -gt 1 ]; then
			add=$(docker exec $srv holdfast host add $fqdn --json) || fail "host add: $add"
			docker exec $srv holdfast job set $fqdn etc --path /etc >/dev/null || fail "job set with the defaults"
			line=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["enrollment"]["install_script"])' <<<"$add")
		fi
		machine $fqdn "$(image_for "$img")"
		out=$(docker exec $fqdn sh -c "$line" 2>&1) || fail "$img: the agent's one line: $out"
		grep -q "enrolled $fqdn" <<<"$out" || fail "$img: $out"
		docker exec $srv holdfast job run $fqdn etc >/dev/null || fail "$img: job run"
		wait_backup $srv $fqdn 0
		ok "$img: the one line installs the agent, enrolls it, and a backup goes through the gateway"

		out=$(docker exec $fqdn sh -c 'curl -sfL file:///dist/install.sh | HOLDFAST_DOWNLOAD_URL=file:///dist sh -' 2>&1) || fail "$img: run again: $out"
		grep -q "upgraded the agent" <<<"$out" || fail "$img: run again: $out"
		docker exec $fqdn systemctl is-active -q holdfast-agent || fail "$img: the agent stopped after running it again"
		ok "$img: run again, it only upgrades, and the agent runs"
	done

	out=$(docker exec $srv sh -c "set -a; . /work/server-$n.env; set +a; curl -sfL file:///dist/install.sh | sh -s - server" 2>&1) || fail "server run again: $out"
	grep -q "upgraded the server" <<<"$out" && grep -q "settings are ignored" <<<"$out" || fail "server run again: $out"
	ok "$srv_image: the server's install run again only upgrades, and says the settings are ignored"

	if $least; then
		out=$(docker exec $srv holdfast notify test 2>&1) || fail "notify test: $out"
		for _ in $(seq 10); do docker logs hf-install-hook 2>&1 | grep -q "^POST /alert-$n .*\"text\"" && break; sleep 1; done
		docker logs hf-install-hook 2>&1 | grep -q "^POST /alert-$n .*\"text\"" || fail "the test alert did not reach the webhook: $out"
		for _ in $(seq 90); do docker logs hf-install-hook 2>&1 | grep -q "^GET /beat-$n " && break; sleep 2; done
		docker logs hf-install-hook 2>&1 | grep -q "^GET /beat-$n " || fail "no heartbeat"
		ok "$srv_image: holdfast notify test reaches the webhook, and the heartbeat is called"
	fi

	out=$(docker exec agent$n-1.install.test sh -c 'curl -sfL file:///dist/install.sh | HOLDFAST_DOWNLOAD_URL=file:///dist sh -s - cli' 2>&1) || fail "cli: $out"
	docker exec agent$n-1.install.test holdfast --help >/dev/null || fail "the CLI does not run"
	ok "the CLI installs"

	# The recovery file goes offline, as the summary says: the problem ends.
	docker exec $srv cat /root/holdfast-recovery.txt >"$work/recovery-$n.txt"
	$least || cat "$work/escrow-key.txt" >>"$work/recovery-$n.txt" # the admin's own age key, added below
	chmod 0644 "$work/recovery-$n.txt" # read by root in the containers; it is the test's own
	docker exec $srv rm /root/holdfast-recovery.txt
	for _ in $(seq 30); do docker exec $srv holdfast status --json | grep -q "\"recovery_file\"" || break; sleep 1; done
	if docker exec $srv holdfast status --json | grep -q "\"recovery_file\""; then fail "recovery_file is still shown with the file gone"; fi
	ok "the recovery file copied off the server and deleted there: the problem ends"

	# The server lost and replaced on a new machine; the agents come back by themselves.
	docker exec $srv holdfast export now >/dev/null || fail "export now"
	$least || docker exec $srv tar -C / -cf - etc/holdfast >"$work/etc-holdfast-$n.tar"
	before=$(backups $srv agent$n-1.install.test)
	docker rm -f $srv >/dev/null
	machine $srv "$(image_for "$srv_image")"
	if $least; then
		# The recovery file and the settings the server was installed with: nothing else.
		out=$(docker exec $srv sh -c "set -a; . /work/server-$n.env; set +a; export HOLDFAST_RECOVERY_FILE=/work/recovery-$n.txt; curl -sfL file:///dist/install.sh | sh -s - server" 2>&1) ||
			fail "rebuild from the recovery file: $out"
		grep -q "restored from the bucket's newest nightly export" <<<"$out" || fail "rebuild from the recovery file: $out"
		[ "$(sed -n 's/^  Agents connect to .*, and pin //p' <<<"$out")" = "$pin" ] || fail "the rebuilt server has another pin: $out"
		how="from the recovery file and the same settings"
	else
		docker exec -i $srv tar -C / -xf - <"$work/etc-holdfast-$n.tar"
		out=$(docker exec $srv sh -c 'curl -sfL file:///dist/install.sh | HOLDFAST_DOWNLOAD_URL=file:///dist sh -s - server' 2>&1) || fail "rebuild, the install: $out"
		grep -q "the server is not started" <<<"$out" || fail "rebuild, the install: $out"
		out=$(docker exec $srv holdfast-server restore-export -identity /work/recovery-$n.txt -latest 2>&1) || fail "restore-export: $out"
		docker exec $srv systemctl enable -q --now holdfast-server
		how="from /etc/holdfast, the same line and restore-export"
	fi
	for _ in $(seq 60); do docker exec $srv holdfast status >/dev/null 2>&1 && break; sleep 1; done
	docker exec $srv holdfast job run agent$n-1.install.test etc >/dev/null || fail "rebuilt: job run"
	wait_backup $srv agent$n-1.install.test "$before"
	ok "$srv_image: the server rebuilt $how; the agent back by itself, and backing up"
done
echo "install test: $pass passed"
