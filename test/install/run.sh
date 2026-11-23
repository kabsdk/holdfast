#!/usr/bin/env bash
# The installer's end-to-end test (decision 044): dist/install.sh run as a person runs it, `curl … | sh`, on machines
# with systemd, against a real S3 server (MinIO), with the packages `make packages` built in dist/. Each scenario is a
# server and one or more agents, each a container of its own on one network:
#   - the installer refuses a server with settings missing, and changes nothing;
#   - the server installs with its own PostgreSQL, CA, keys, the web UI's settings, and starts;
#   - `holdfast host add` prints the one line; on each agent it installs, enrolls, and a backup goes through the gateway;
#   - run again, it only upgrades, on the server and on an agent;
#   - the witness and the CLI install;
#   - the server lost: on a new machine with /etc/holdfast put back, the same line prepares it, restore-export loads the
#     nightly export, and the agent comes back by itself and backs up.
# Usage: test/install/run.sh [<server image>:<agent image>[,<agent image>…] …]     (make install-test)
# KEEP=1 leaves the containers running for a look.
set -euo pipefail
cd "$(dirname "$0")/../.."

MINIO_IMAGE=${MINIO_IMAGE:?} MC_IMAGE=${MC_IMAGE:?}
scenarios=("$@")
[ ${#scenarios[@]} -gt 0 ] || scenarios=("almalinux:9:ubuntu:24.04,oraclelinux:9" "ubuntu:24.04:almalinux:9")
[ -s dist/install.sh ] && [ -s dist/VERSION ] || { echo "dist/ has no release: make packages first" >&2; exit 1; }

net=hf-install
work=$(mktemp -d)
chmod 0755 "$work" # mc's image runs as a user of its own; what is here is the test's own throwaway material
containers=()
cleanup() {
	if [ "${KEEP:-}" = 1 ]; then echo "KEEP=1: containers ${containers[*]} and $work are left"; return; fi
	docker rm -f "${containers[@]}" hf-install-minio >/dev/null 2>&1 || true
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

docker network create $net >/dev/null
docker run -d --name hf-install-minio --network $net -e MINIO_ROOT_USER=hfroot -e MINIO_ROOT_PASSWORD=hfroot-install-secret "$MINIO_IMAGE" server /data >/dev/null
for _ in $(seq 30); do mcx ready hf >/dev/null 2>&1 && break; sleep 1; done
GOTOOLCHAIN=local go run ./lab/escrowkey "$work/escrow-key.txt" >/dev/null
recipient=$(sed -n 's/^# public key: //p' "$work/escrow-key.txt")
chmod 0644 "$work/escrow-key.txt" # read by root in the containers; it is a throwaway key
# The UI's certificate, made here: a self-signed one is enough to see the UI's listener answer.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=hfsrv -days 2 -keyout "$work/ui.key" -out "$work/ui.crt" 2>/dev/null
chmod 0644 "$work/ui.key"

n=0
for sc in "${scenarios[@]}"; do
	n=$((n + 1))
	srv_image=$(echo "$sc" | sed -E 's/^([^:]+:[^:]+):.*/\1/')
	agent_images=$(echo "$sc" | sed -E 's/^[^:]+:[^:]+://' | tr ',' ' ')
	bucket=holdfast-$n srv=hfsrv$n.install.test
	echo "== scenario $n: server on $srv_image, agents on $agent_images"

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
  {"Effect": "Allow", "Action": ["s3:PutObject", "s3:DeleteObject"], "Resource": ["arn:aws:s3:::$bucket/escrow/*"]},
  {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"], "Resource": ["arn:aws:s3:::$bucket/meta/*"]},
  {"Effect": "Deny", "Action": ["s3:DeleteObjectVersion"], "Resource": ["arn:aws:s3:::$bucket/*"]}]}
EOF
	cat >"$work/witness-$n.json" <<EOF
{"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Action": ["s3:ListBucket"], "Resource": ["arn:aws:s3:::$bucket"]}]}
EOF
	for k in gw maint witness; do
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
HOLDFAST_ESCROW_RECIPIENT=$recipient
HOLDFAST_UI_URL=https://$srv
HOLDFAST_UI_TLS_CERT=/work/ui.crt
HOLDFAST_UI_TLS_KEY=/work/ui.key
HOLDFAST_OIDC_ISSUER=https://auth.install.test
HOLDFAST_OIDC_CLIENT_ID=holdfast
HOLDFAST_OIDC_CLIENT_SECRET=oidc-$n-install-secret
HOLDFAST_ADMINS=alice
EOF
	cat >"$work/witness-$n.env" <<EOF
HOLDFAST_DOWNLOAD_URL=file:///dist
HOLDFAST_S3_ENDPOINT=http://hf-install-minio:9000
HOLDFAST_S3_BUCKET=$bucket
HOLDFAST_WITNESS_ACCESS_KEY_ID=witness$n
HOLDFAST_WITNESS_SECRET_ACCESS_KEY=witness-$n-install-secret
EOF

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
	pin=$(sed -n 's/^  Agents pin *//p' <<<"$out")
	[[ $pin =~ ^[0-9a-f]{64}$ ]] || fail "the server's install printed no pin: $out"
	docker exec $srv holdfast status >/dev/null || fail "holdfast status after the install"
	docker exec $srv sh -c 'stat -c "%a %n" /etc/holdfast /etc/holdfast/*.env /etc/holdfast/db.dsn /etc/holdfast/credentials.key /etc/holdfast/oidc-client-secret /etc/holdfast/tls/ca.key /etc/holdfast/tls/server.key' >"$work/modes" ||
		fail "the server's files"
	if grep -vE '^(700|600) ' "$work/modes"; then fail "a secret is readable by others"; fi
	if docker exec $srv grep -rlE 'install-secret' /etc/holdfast/server.yaml; then fail "a secret is in server.yaml"; fi
	code=$(docker exec $srv curl -sk -o /dev/null -w '%{http_code}' https://localhost/)
	[[ $code =~ ^(200|30[23])$ ]] || fail "the UI answered $code"
	ok "$srv_image: the server installs with PostgreSQL, its CA ($pin), keys root-only, the UI on :443"

	add=$(docker exec $srv holdfast host add agent$n-1.install.test --job etc --path /etc --daily 02:00 \
		--keep-daily 14d --keep-weekly 8w --keep-monthly 12mo --keep-yearly 3y --json) || fail "host add: $add"
	line=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["enrollment"]["install_script"])' <<<"$add")
	[[ $line == "curl -sfL file:///dist/install.sh | HOLDFAST_SERVER=https://$srv:8443 HOLDFAST_FQDN=agent$n-1.install.test "*" HOLDFAST_CA_SHA256=$pin HOLDFAST_DOWNLOAD_URL=file:///dist sh -" ]] ||
		fail "host add's one line: $line"
	ok "host add prints the one line: $line"

	i=0
	for img in $agent_images; do
		i=$((i + 1))
		fqdn=agent$n-$i.install.test
		if [ $i -gt 1 ]; then
			add=$(docker exec $srv holdfast host add $fqdn --job etc --path /etc --daily 02:00 \
				--keep-daily 14d --keep-weekly 8w --keep-monthly 12mo --keep-yearly 3y --json) || fail "host add: $add"
			line=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["enrollment"]["install_script"])' <<<"$add")
		fi
		machine $fqdn "$(image_for "$img")"
		out=$(docker exec $fqdn sh -c "$line" 2>&1) || fail "$img: the agent's one line: $out"
		grep -q "enrolled $fqdn" <<<"$out" || fail "$img: $out"
		docker exec $srv holdfast job run $fqdn etc >/dev/null
		for _ in $(seq 120); do
			docker exec $srv holdfast host events $fqdn --json | grep -q '"status": *"success"' && break
			sleep 2
		done
		docker exec $srv holdfast host events $fqdn --json | grep -q '"status": *"success"' || fail "$img: no backup: $(docker exec $srv holdfast host events $fqdn)"
		ok "$img: the one line installs the agent, enrolls it, and a backup goes through the gateway"

		out=$(docker exec $fqdn sh -c 'curl -sfL file:///dist/install.sh | HOLDFAST_DOWNLOAD_URL=file:///dist sh -' 2>&1) || fail "$img: run again: $out"
		grep -q "upgraded the agent" <<<"$out" || fail "$img: run again: $out"
		docker exec $fqdn systemctl is-active -q holdfast-agent || fail "$img: the agent stopped after running it again"
		ok "$img: run again, it only upgrades, and the agent runs"
	done

	out=$(docker exec $srv sh -c "set -a; . /work/server-$n.env; set +a; curl -sfL file:///dist/install.sh | sh -s - server" 2>&1) || fail "server run again: $out"
	grep -q "upgraded the server" <<<"$out" && grep -q "settings are ignored" <<<"$out" || fail "server run again: $out"
	ok "$srv_image: the server's install run again only upgrades, and says the settings are ignored"

	out=$(docker exec $srv sh -c "set -a; . /work/witness-$n.env; set +a; curl -sfL file:///dist/install.sh | sh -s - witness" 2>&1) || fail "witness: $out"
	docker exec $srv systemctl is-enabled -q holdfast-freshness || fail "the witness is not enabled"
	[ "$(docker exec $srv stat -c %a /etc/holdfast/witness.env)" = 600 ] || fail "the witness's key file is readable by others"
	docker exec $srv grep -qF -- "-endpoint http://hf-install-minio:9000 -bucket $bucket -credentials-file \${CREDENTIALS_DIRECTORY}/witness.env" \
		/etc/systemd/system/holdfast-freshness.service.d/install.conf || fail "the witness's unit: $(docker exec $srv cat /etc/systemd/system/holdfast-freshness.service.d/install.conf)"
	# systemd hands the key to the witness's dynamic user in /run/credentials, which a container's /run cannot do (on
	# the lab's AlmaLinux VM it does). So the witness runs once here with the settings the installer wrote.
	m=$(docker exec $srv /usr/bin/holdfast-freshness -endpoint http://hf-install-minio:9000 -bucket $bucket -credentials-file /etc/holdfast/witness.env -once) ||
		fail "the witness with the installer's settings: $m"
	grep -q "^holdfast_storage_newest_snapshot_timestamp_seconds{.*agent$n-1.install.test" <<<"$m" || fail "the witness sees no snapshot: $m"
	ok "$srv_image: the witness installs, and lists the bucket with the key it was given"

	out=$(docker exec agent$n-1.install.test sh -c 'curl -sfL file:///dist/install.sh | HOLDFAST_DOWNLOAD_URL=file:///dist sh -s - cli' 2>&1) || fail "cli: $out"
	docker exec agent$n-1.install.test holdfast --help >/dev/null || fail "the CLI does not run"
	ok "the CLI installs"

	# The server lost and replaced, as the docs have it: /etc/holdfast from its offline copy on a new machine, the same
	# one line, restore-export, and the agents back by themselves.
	docker exec $srv holdfast export now >/dev/null || fail "export now"
	docker exec $srv tar -C / -cf - etc/holdfast >"$work/etc-holdfast-$n.tar"
	before=$(docker exec $srv holdfast host events agent$n-1.install.test --json | grep -o '"status": *"success"' | wc -l)
	docker rm -f $srv >/dev/null
	machine $srv "$(image_for "$srv_image")"
	docker exec -i $srv tar -C / -xf - <"$work/etc-holdfast-$n.tar"
	out=$(docker exec $srv sh -c 'curl -sfL file:///dist/install.sh | HOLDFAST_DOWNLOAD_URL=file:///dist sh -s - server' 2>&1) || fail "rebuild, the install: $out"
	grep -q "the server is not started" <<<"$out" || fail "rebuild, the install: $out"
	out=$(docker exec $srv holdfast-server restore-export -identity /work/escrow-key.txt -latest 2>&1) || fail "restore-export: $out"
	docker exec $srv systemctl enable -q --now holdfast-server
	for _ in $(seq 60); do docker exec $srv holdfast status >/dev/null 2>&1 && break; sleep 1; done
	docker exec $srv holdfast job run agent$n-1.install.test etc >/dev/null || fail "rebuilt: job run"
	for _ in $(seq 90); do
		now=$(docker exec $srv holdfast host events agent$n-1.install.test --json | grep -o '"status": *"success"' | wc -l)
		[ "$now" -gt "$before" ] && break
		sleep 2
	done
	[ "$now" -gt "$before" ] || fail "rebuilt: the agent did not back up: $(docker exec $srv holdfast host events agent$n-1.install.test)"
	ok "$srv_image: the server rebuilt from /etc/holdfast, the same line and the export; the agent back by itself, and backing up"
done
echo "install test: $pass passed"
