#!/usr/bin/env bash
# lab/test-rebuild.sh <lab> [<vm>...]: M9's lab test (plan-m9 Q8): docs/runbooks/rebuild-holdfast.md walked on the lab's
# server. lab/lab test runs it after the M8 test, which leaves each agent enrolled with snapshots. One server and no
# standby (decision 042): the server is lost and replaced, on the same machine, from the nightly export.
#   1. holdfast export now; each agent's snapshots listed;
#   2. the admin's offline copy of /etc/holdfast kept in the lab's state (server.yaml, the key files, the TLS files and
#      the credentials key, as the runbook says to keep them);
#   3. the server lost: holdfast-server stopped, its database dropped and made again empty, its packages removed, and
#      /etc/holdfast and /var/lib/holdfast-server deleted;
#   4. the runbook: the packages installed again, /etc/holdfast put back, holdfast-server restore-export -latest with
#      the escrow's age key (copied to the server for the restore and deleted after), holdfast-server started;
#   5. each agent back by itself, with no new code, its snapshots listed as before; a backup through the gateway on
#      HF_REBUILD_VM (default: the first agent named or of the lab).
# The results are in <state>/test-rebuild.txt. Exit 0 when everything passed, 1 otherwise, 2 misused, 3 the lab is not
# up. Secrets travel on SSH's stdin only.
set -euo pipefail
cd "$(dirname "$0")/.."
lab=${1:-}
[[ $lab =~ ^[a-z0-9]+$ ]] || { sed -n '2,/^set -e/{/^set -e/d; s/^# \{0,1\}//; p}' "$0" >&2; exit 2; }
shift
state=${XDG_STATE_HOME:-$HOME/.local/state}/holdfast/labs/$lab
[[ -f $state/lab.env ]] || { echo "test-rebuild: the lab $lab is not up (lab/lab up $lab)" >&2; exit 3; }
# shellcheck source=/dev/null
source "$state/lab.env"
vms=("$@")
((${#vms[@]})) || vms=("${HF_AGENTS[@]}")
backup_vm=${HF_REBUILD_VM:-${vms[0]}}
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
js() { python3 -I -c "import json, sys
j = json.load(sys.stdin)
$1" "${@:2}"; }
results=()
result() { results+=("$(printf '%-6s %-24s %s' "$1" "$2" "$3")"); say "  $1: $2: $3"; }
finish() {
	{
		echo "M9's lab test, $(date -u +%FT%TZ): Holdfast $version"
		echo
		printf '%s\n' "${results[@]}"
		echo
		fails=$(printf '%s\n' "${results[@]}" | grep -c '^FAIL' || true)
		if ((fails == 0)); then echo "result: PASS, ${#results[@]} of ${#results[@]}"; else echo "result: FAIL, $fails of ${#results[@]} failed"; fi
	} | tee "$HF_STATE/test-rebuild.txt" >&3
	! grep -q '^FAIL' <<<"$(printf '%s\n' "${results[@]}")"
	exit $?
}
present() { js 'print(" ".join(sorted(s["id"] for s in j or [] if not s.get("gone_at"))))'; }

# 1. The export, and what each agent has.
say "holdfast export now on $HF_SERVER_VM"
if ! out=$(hf export now --json); then
	result FAIL "export" "holdfast export now failed: $out"
	finish
fi
name=$(js 'print(j["name"])' <<<"$out")
result PASS "export" "$(js 'print("meta/%s, %d KiB%s" % (j["name"], j["bytes"] // 1024, ", off-site too" if j.get("offsite") and not j.get("offsite_error") else ""))' <<<"$out")"
declare -A before
for vm in "${vms[@]}"; do
	before[$vm]=$(hf snapshot list "$vm.$HF_DOMAIN" --refresh --json | present)
done

# 2. The admin's offline copy of /etc/holdfast, in the lab's state.
mkdir -p "$HF_STATE/rebuild" && chmod 700 "$HF_STATE/rebuild"
copy=$HF_STATE/rebuild/etc-holdfast.tar
(umask 077; vm_ssh "$HF_SERVER_VM" 'tar -C / -cf - etc/holdfast' </dev/null >"$copy")
if ! tar -tf "$copy" | grep -qx 'etc/holdfast/credentials.key' || ! tar -tf "$copy" | grep -qx 'etc/holdfast/server.yaml'; then
	result FAIL "offline copy" "the copy of /etc/holdfast lacks credentials.key or server.yaml"
	finish
fi

# 3. The server is lost.
pkgver=$(vm_ssh "$HF_SERVER_VM" "rpm -q --qf '%{VERSION}-%{RELEASE}' holdfast-server" </dev/null)
say "$HF_SERVER_VM: the server lost: holdfast-server stopped, database holdfast dropped, packages removed, /etc/holdfast and /var/lib/holdfast-server deleted"
if ! vm_ssh "$HF_SERVER_VM" bash -s <<'EOF'; then
set -euo pipefail
systemctl stop holdfast-server
runuser -u postgres -- psql -X -q -v ON_ERROR_STOP=1 -c 'DROP DATABASE holdfast WITH (FORCE)' -c 'CREATE DATABASE holdfast OWNER holdfast'
rpm -e holdfast-server holdfast-freshness holdfast-cli holdfast-restic
rm -rf /etc/holdfast /var/lib/holdfast-server
EOF
	result FAIL "server lost" "wiping $HF_SERVER_VM failed: the log has why"
	finish
fi
result PASS "server lost" "database dropped and made again empty; packages, /etc/holdfast and /var/lib/holdfast-server removed"

# 4. The runbook.
say "$HF_SERVER_VM: the packages $pkgver installed again, /etc/holdfast put back, restore-export -latest, holdfast-server started"
pkgs=$(printf '/var/tmp/holdfast-packages/holdfast-%s-'"$pkgver"'.x86_64.rpm ' restic cli server freshness)
if ! vm_ssh "$HF_SERVER_VM" "rpm -i $pkgs" </dev/null || ! vm_ssh "$HF_SERVER_VM" 'tar -C / -xpf -' <"$copy"; then
	result FAIL "reinstalled" "installing the packages or putting /etc/holdfast back failed"
	finish
fi
result PASS "reinstalled" "holdfast-restic, -cli, -server and -freshness $pkgver installed again; /etc/holdfast put back"
vm_ssh "$HF_SERVER_VM" 'umask 077; cat >/root/hf-escrow-age.key' <"$HF_STATE/credentials/escrow-age.key"
rc=0
out=$(vm_ssh "$HF_SERVER_VM" 'holdfast-server restore-export -identity /root/hf-escrow-age.key -latest; rc=$?; rm -f /root/hf-escrow-age.key; exit $rc' </dev/null 2>&1) || rc=$?
echo "$out"
if [[ $rc != 0 ]] || ! grep -q '^restored: head revision' <<<"$out"; then
	result FAIL "restored" "restore-export failed (exit $rc): $(tail -1 <<<"$out")"
	finish
fi
result PASS "restored" "from meta/$name: $(sed -n 's/^restored: \(head revision [0-9]*, [0-9]* hosts\).*/\1/p' <<<"$out")"
vm_ssh "$HF_SERVER_VM" 'systemctl enable --now holdfast-server' </dev/null
for ((i = 0; i < 60; i++)); do hf status >/dev/null 2>&1 && break; sleep 2; done

# 5. The agents, back by themselves.
for vm in "${vms[@]}"; do
	fqdn=$vm.$HF_DOMAIN
	online=
	for ((i = 0; i < 60; i++)); do
		online=$(hf host show "$fqdn" --json 2>/dev/null | js 'print(j.get("online") and j.get("state") == "enrolled")' 2>/dev/null || true)
		[[ $online == True ]] && break
		sleep 5
	done
	if [[ $online != True ]]; then
		result FAIL "$vm back" "the agent did not come back within 5 minutes"
		continue
	fi
	now=$(hf snapshot list "$fqdn" --refresh --json | present)
	if [[ $now == "${before[$vm]}" ]]; then
		result PASS "$vm back" "online by itself, with no new code; $(wc -w <<<"$now") of $(wc -w <<<"${before[$vm]}") snapshots listed"
	else
		result FAIL "$vm back" "online, but its snapshots differ: $(wc -w <<<"$now") now, $(wc -w <<<"${before[$vm]}") before"
	fi
done
fqdn=$backup_vm.$HF_DOMAIN
say "$backup_vm: holdfast job run $fqdn fidelity"
# The run's own event, by its command's ID: the event list is the newest only, and the agent's catch-up backups after
# the outage are in it too.
cmd=$(hf job run "$fqdn" fidelity --json | js 'print(j["command"])')
status=
for ((i = 0; i < 60; i++)); do
	status=$(hf host events "$fqdn" --json | js 'r = [e for e in j or [] if e.get("command_id") == sys.argv[1]]
print(r[0]["status"] if r else "")' "$cmd")
	[[ -n $status ]] && break
	sleep 5
done
if [[ $status == success ]]; then
	result PASS "$backup_vm backup" "backed up through the gateway on the rebuilt server"
else
	result FAIL "$backup_vm backup" "the backup ${status:-did not finish within 5 minutes}"
fi
if out=$(hf export list --json) && [[ $(js 'print((j.get("restored") or {}).get("export_name"))' <<<"$out") == "$name" ]]; then
	result PASS "export list" "says the server was restored from $name"
else
	result FAIL "export list" "does not say the server was restored from $name"
fi
finish
