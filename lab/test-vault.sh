#!/usr/bin/env bash
# lab/test-vault.sh <lab> [<vm>...]: M7's lab test (plan-m7 §3). lab/lab test runs it after the M6 tests, which leave each
# agent enrolled with its fidelity snapshot. Site B is a second bucket on the lab's RGW, holdfast-offsite
# (lab/rgw-vault-layout.sh), and holdfast-vault runs on the server VM beside holdfast-server, with a database of its own:
#   1. the keys, made once in the lab's state: holdfast-vault-read (site A: read repos/ only), holdfast-vault-maint
#      (site B's maintenance key) and hfb-<vm> (site B: the restore key for <vm>'s prefix), put on the storage VM; then
#      rgw-documented-layout.sh --keep-data (the read user in site A's bucket policy) and rgw-vault-layout.sh;
#   2. on the server VM: the database holdfast_vault; the vault registered on site A with holdfast vault add site-b, its
#      read key on stdin (a vault of that name is removed first, so its token is new); /etc/holdfast-vault/ with the
#      token, the credentials key (once), site B's maintenance key, the DSN and vault.yaml; holdfast-vault.service
#      started;
#   3. the vault copies each agent's newest fidelity snapshot (holdfast --vault snapshot list: a copy of it, made by
#      Holdfast), and site A's host view shows the copies;
#   4. on HF_VAULT_VM (default: the first agent named or of the lab): its restore key given to the vault (holdfast --vault
#      host set-key, on stdin); the copy restored through the agent from site B (holdfast restore --from-vault site-b
#      --wait); hf-fidelity compare of the fixture with the restored one.
# The results are in <state>/test-vault.txt. Exit 0 when everything passed, 1 otherwise, 2 misused, 3 the lab is not up.
# Keys and the token travel on SSH's stdin only.
set -euo pipefail
cd "$(dirname "$0")/.."
lab=${1:-}
[[ $lab =~ ^[a-z0-9]+$ ]] || { sed -n '2,/^set -e/{/^set -e/d; s/^# \{0,1\}//; p}' "$0" >&2; exit 2; }
shift
state=${XDG_STATE_HOME:-$HOME/.local/state}/holdfast/labs/$lab
[[ -f $state/lab.env ]] || { echo "test-vault: the lab $lab is not up (lab/lab up $lab)" >&2; exit 3; }
# shellcheck source=/dev/null
source "$state/lab.env"
vms=("$@")
((${#vms[@]})) || vms=("${HF_AGENTS[@]}")
restore_vm=${HF_VAULT_VM:-${vms[0]}}
creds=$HF_STATE/credentials
pin=$(cat "$HF_STATE/ca/ca.sha256")
version=$(cat dist/VERSION)
{ true >&3; } 2>/dev/null || exec 3>&1
say() { echo "== $*"; echo "lab: $*" >&3; }
vm_ssh() {
	local vm=$1 ip
	shift
	ip=HF_VM_IP_$vm
	ssh -i "$HF_SSH_KEY" -o IdentitiesOnly=yes -o "UserKnownHostsFile=$HF_STATE/known_hosts" -o StrictHostKeyChecking=accept-new \
		-o LogLevel=ERROR -o BatchMode=yes -o ConnectTimeout=10 "root@${!ip}" "$@"
}
hf() { vm_ssh "$HF_SERVER_VM" "$(printf '%q ' holdfast "$@")" </dev/null; }
hfv() { vm_ssh "$HF_SERVER_VM" "$(printf '%q ' holdfast --vault "$@")" </dev/null; }
js() { python3 -I -c "import json, sys
j = json.load(sys.stdin)
$1" "${@:2}"; }
results=()
result() { results+=("$(printf '%-6s %-30s %s' "$1" "$2" "$3")"); say "  $1: $2: $3"; }
finish() {
	{
		echo "M7's lab test, $(date -u +%FT%TZ): Holdfast $version"
		echo
		printf '%s\n' "${results[@]}"
		echo
		fails=$(printf '%s\n' "${results[@]}" | grep -c '^FAIL' || true)
		if ((fails == 0)); then echo "result: PASS, ${#results[@]} of ${#results[@]}"; else echo "result: FAIL, $fails of ${#results[@]} failed"; fi
	} | tee "$HF_STATE/test-vault.txt" >&3
	! grep -q '^FAIL' <<<"$(printf '%s\n' "${results[@]}")"
	exit $?
}

# 1. The keys and the buckets.
say "the vault's keys, site A's bucket policy and site B's bucket (rgw-documented-layout.sh --keep-data, rgw-vault-layout.sh on $HF_STORAGE_VM)"
python3 lab/ansible/files/credential.py s3 "$creds/holdfast-vault-read.env" HFVREAD
python3 lab/ansible/files/credential.py s3 "$creds/holdfast-vault-maint.env" HFVMAINT
offsite=
for vm in "${HF_AGENTS[@]}"; do
	python3 lab/ansible/files/credential.py s3 "$creds/hfb-$vm.env" "HFB${vm^^}"
	vm_ssh "$HF_STORAGE_VM" "umask 077 && cat >/root/hf-lab/hfb-$vm.env" <"$creds/hfb-$vm.env"
	offsite+="hfb-$vm $vm.$HF_DOMAIN"$'\n'
done
for u in holdfast-vault-read holdfast-vault-maint; do
	vm_ssh "$HF_STORAGE_VM" "umask 077 && cat >/root/hf-lab/$u.env" <"$creds/$u.env"
done
vm_ssh "$HF_STORAGE_VM" "umask 077 && cat >/root/hf-lab/readers" <<<"holdfast-vault-read"
vm_ssh "$HF_STORAGE_VM" "umask 077 && cat >/root/hf-lab/offsite-hosts" <<<"$offsite"
if ! vm_ssh "$HF_STORAGE_VM" 'bash -s -- --keep-data' <lab/rgw-documented-layout.sh || ! vm_ssh "$HF_STORAGE_VM" 'bash -s' <lab/rgw-vault-layout.sh; then
	result FAIL "storage" "the RGW layout failed: the log has why"
	finish
fi
result PASS "storage" "site B is bucket holdfast-offsite; the vault's read user may only read site A's repos/"

# 2. The vault on the server VM.
say "the vault on $HF_SERVER_VM: its database, its registration on site A, /etc/holdfast-vault and holdfast-vault.service"
if ! vm_ssh "$HF_SERVER_VM" bash -s <<'EOF'; then
set -eu
cd /
f=/var/lib/pgsql/data/pg_hba.conf
if ! grep -q holdfast_vault "$f"; then
	printf 'local\tholdfast_vault\tholdfast\t\tscram-sha-256\nhost\tholdfast_vault\tholdfast\t127.0.0.1/32\tscram-sha-256\n' >>"$f"
	systemctl reload postgresql
fi
[ "$(runuser -u postgres -- psql -X -tA -c "SELECT 1 FROM pg_database WHERE datname = 'holdfast_vault'")" = 1 ] ||
	runuser -u postgres -- psql -X -q -c 'CREATE DATABASE holdfast_vault OWNER holdfast'
install -d -m 0700 /etc/holdfast-vault /etc/holdfast-vault/tls
sed 's#/holdfast?#/holdfast_vault?#' /etc/holdfast/db.dsn >/etc/holdfast-vault/db.dsn
chmod 0600 /etc/holdfast-vault/db.dsn
install -m 0600 /etc/holdfast/tls/server.crt /etc/holdfast/tls/server.key /etc/holdfast-vault/tls/
[ -s /etc/holdfast-vault/credentials.key ] || holdfast-vault keygen /etc/holdfast-vault/credentials.key >/dev/null
EOF
	result FAIL "vault set-up" "the database or /etc/holdfast-vault failed: the log has why"
	finish
fi
hf vault remove site-b -m "lab: registered again" >/dev/null 2>&1 || true
if ! out=$(vm_ssh "$HF_SERVER_VM" "holdfast vault add site-b --s3-key-file - --json -m 'lab: the vault'" <"$creds/holdfast-vault-read.env"); then
	result FAIL "vault set-up" "holdfast vault add failed: $out"
	finish
fi
js 'print(j["token"])' <<<"$out" | vm_ssh "$HF_SERVER_VM" "umask 077 && cat >/etc/holdfast-vault/token"
out=
vm_ssh "$HF_SERVER_VM" "umask 077 && cat >/etc/holdfast-vault/maintenance.env" <"$creds/holdfast-vault-maint.env"
if ! vm_ssh "$HF_SERVER_VM" bash -s -- "$HF_SERVER_VM.$HF_DOMAIN" "$pin" <<'EOF'; then
set -eu
fqdn=$1 pin=$2
s=/etc/holdfast/server.yaml
endpoint=$(sed -n 's/^  endpoint: //p' "$s") region=$(sed -n 's/^  region: //p' "$s") recipient=$(sed -n 's/^  age_recipient: //p' "$s")
umask 077
cat >/etc/holdfast-vault/vault.yaml <<YAML
# The lab's vault (lab/test-vault.sh writes it): site B is the bucket holdfast-offsite on the lab's RGW.
role: vault
listen: ":8444"
public_url: https://$fqdn:8444
tls: {cert_file: /etc/holdfast-vault/tls/server.crt, key_file: /etc/holdfast-vault/tls/server.key}
api_socket: /run/holdfast-vault/api.sock
database_dsn_file: /etc/holdfast-vault/db.dsn
credentials_key_file: /etc/holdfast-vault/credentials.key
state_dir: /var/lib/holdfast-vault
restic_bin: /usr/libexec/holdfast/restic
default_zone: UTC
storage: {endpoint: $endpoint, bucket: holdfast-offsite, region: $region, maintenance_credentials_file: /etc/holdfast-vault/maintenance.env}
source: {server: "https://$fqdn:8443", ca_sha256: "$pin", token_file: /etc/holdfast-vault/token, poll: 30s}
maintenance: {check: daily, check_cycle_days: 30, retention: daily}
escrow: {age_recipient: $recipient}
YAML
systemctl enable -q holdfast-vault
systemctl restart holdfast-vault
for i in $(seq 1 30); do [ -S /run/holdfast-vault/api.sock ] && break; sleep 1; done
holdfast --vault status >/dev/null
EOF
	result FAIL "vault set-up" "holdfast-vault did not start: $(vm_ssh "$HF_SERVER_VM" 'journalctl -u holdfast-vault -n 20 --no-pager -o cat' </dev/null || true)"
	finish
fi
result PASS "vault set-up" "registered on site A; holdfast-vault runs on $HF_SERVER_VM, copying every 30s"

# 3. The copies.
declare -A copy
for vm in "${vms[@]}"; do
	fqdn=$vm.$HF_DOMAIN
	out=$(hf snapshot list "$fqdn" --json) || { result FAIL "$vm copy" "holdfast snapshot list failed"; continue; }
	snap=$(js 'r = [s for s in j or [] if s.get("job_id") == "fidelity" and not s.get("gone_at")]
print(r[0]["id"] if r else "")' <<<"$out")
	[[ -n $snap ]] || { result FAIL "$vm copy" "no fidelity snapshot in site A (run the fidelity test first)"; continue; }
	c=
	for ((i = 0; i < 60; i++)); do
		if out=$(hfv snapshot list "$fqdn" --json 2>/dev/null); then
			c=$(js 'r = [s for s in j or [] if s.get("original") == sys.argv[1] and s.get("made_by_holdfast") and not s.get("gone_at")]
print(r[0]["id"] if r else "")' "$snap" <<<"$out")
			[[ -z $c ]] || break
		fi
		sleep 10
	done
	[[ -n $c ]] || { result FAIL "$vm copy" "no copy of ${snap:0:8} in site B within 10 minutes"; continue; }
	copy[$vm]=$c
	n=0
	for ((i = 0; i < 30; i++)); do
		n=$(hf host show "$fqdn" --json | js 'print(sum(o.get("copies", 0) for o in j.get("offsite") or []))')
		((n > 0)) && break
		sleep 5
	done
	if ((n > 0)); then
		result PASS "$vm copy" "${snap:0:8} copied to site B as ${c:0:8}; site A sees $n copies"
	else
		result FAIL "$vm copy" "${snap:0:8} copied as ${c:0:8}, but site A's host view shows no copies"
	fi
done

# 4. A restore from site B.
vm=$restore_vm fqdn=$restore_vm.$HF_DOMAIN
if [[ -z ${copy[$vm]:-} ]]; then
	result FAIL "$vm restore from B" "no copy to restore"
	finish
fi
say "$vm: its restore key to the vault, then holdfast restore $fqdn ${copy[$vm]:0:8} --from-vault site-b --wait"
if ! vm_ssh "$HF_SERVER_VM" "holdfast --vault host set-key $fqdn --s3-key-file - >/dev/null" <"$creds/hfb-$vm.env"; then
	result FAIL "$vm restore from B" "holdfast --vault host set-key failed"
	finish
fi
rc=0
out=$(vm_ssh "$HF_SERVER_VM" "timeout 1800 holdfast restore $fqdn ${copy[$vm]:0:12} --from-vault site-b --wait --json -m 'lab: from site B'" </dev/null) || rc=$?
echo "the restore's event (exit $rc): $out"
if [[ $rc != 0 ]]; then
	result FAIL "$vm restore from B" "the restore failed: $out"
	finish
fi
dir=$(js 'print(j["detail"]["directory"])' <<<"$out")
rc=0
vm_ssh "$vm" "/usr/local/bin/hf-fidelity compare --json /srv/hf-fixture $dir/srv/hf-fixture >/root/hf-vault-compare.json" </dev/null || rc=$?
cmp=$(vm_ssh "$vm" 'cat /root/hf-vault-compare.json' </dev/null || true)
echo "$cmp"
kept=$(js 'print(sum(r["pass"] for r in j), len(j))' <<<"${cmp:-[]}" 2>/dev/null || echo "0 0")
if [[ $kept == "13 13" ]]; then
	result PASS "$vm restore from B" "restored from site B into $dir: 13 of 13 properties kept"
else
	result FAIL "$vm restore from B" "${kept% *} of 13 properties kept (exit $rc)"
fi
finish
