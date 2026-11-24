#!/usr/bin/env bash
# lab/test-offsite.sh <lab> [<vm>...]: M8's off-site lab test. lab/lab test runs it after the M6
# tests, which leave each agent enrolled with its fidelity snapshot. The off-site bucket is a second bucket on the lab's
# RGW, holdfast-offsite (lab/rgw-offsite-layout.sh, which lab/test-fidelity.sh runs), and holdfast-server copies into it
# itself (offsite: in its server.yaml, decision 041): there is no other program and nothing to register.
#   1. the server copies each agent's newest fidelity snapshot (holdfast --offsite snapshot list: a copy of it, made by
#      Holdfast), and the host's page shows the copies;
#   2. without a restore, the host cannot read its off-site copy through the gateway (403);
#   3. on HF_OFFSITE_VM (default: the first agent named or of the lab): the copy restored through the agent from the
#      off-site bucket (holdfast restore --from-offsite --wait), which reads it through the gateway; hf-fidelity compare
#      of the fixture with the restored one.
# The results are in <state>/test-offsite.txt. Exit 0 when everything passed, 1 otherwise, 2 misused, 3 the lab is not
# up.
set -euo pipefail
cd "$(dirname "$0")/.."
lab=${1:-}
[[ $lab =~ ^[a-z0-9]+$ ]] || { sed -n '2,/^set -e/{/^set -e/d; s/^# \{0,1\}//; p}' "$0" >&2; exit 2; }
shift
state=${XDG_STATE_HOME:-$HOME/.local/state}/holdfast/labs/$lab
[[ -f $state/lab.env ]] || { echo "test-offsite: the lab $lab is not up (lab/lab up $lab)" >&2; exit 3; }
# shellcheck source=/dev/null
source "$state/lab.env"
vms=("$@")
((${#vms[@]})) || vms=("${HF_AGENTS[@]}")
restore_vm=${HF_OFFSITE_VM:-${vms[0]}}
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
hfo() { vm_ssh "$HF_SERVER_VM" "$(printf '%q ' holdfast --offsite "$@")" </dev/null; }
js() { python3 -I -c "import json, sys
j = json.load(sys.stdin)
$1" "${@:2}"; }
results=()
result() { results+=("$(printf '%-6s %-30s %s' "$1" "$2" "$3")"); say "  $1: $2: $3"; }
finish() {
	{
		echo "M8's off-site lab test, $(date -u +%FT%TZ): Holdfast $version"
		echo
		printf '%s\n' "${results[@]}"
		echo
		fails=$(printf '%s\n' "${results[@]}" | grep -c '^FAIL' || true)
		if ((fails == 0)); then echo "result: PASS, ${#results[@]} of ${#results[@]}"; else echo "result: FAIL, $fails of ${#results[@]} failed"; fi
	} | tee "$HF_STATE/test-offsite.txt" >&3
	! grep -q '^FAIL' <<<"$(printf '%s\n' "${results[@]}")"
	exit $?
}

# 0. Off-site copies are on.
if ! out=$(hf settings --json) || [[ $(js 'print(j.get("offsite"))' <<<"$out") != True ]]; then
	result FAIL "off-site" "the server does not copy off-site: server.yaml has no offsite: section"
	finish
fi
result PASS "off-site" "holdfast-server copies to holdfast-offsite, every 30s"

# 1. The copies.
declare -A copy
for vm in "${vms[@]}"; do
	fqdn=$vm.$HF_DOMAIN
	out=$(hf snapshot list "$fqdn" --json) || { result FAIL "$vm copy" "holdfast snapshot list failed"; continue; }
	snap=$(js 'r = [s for s in j or [] if s.get("job_id") == "fidelity" and not s.get("gone_at")]
print(r[0]["id"] if r else "")' <<<"$out")
	[[ -n $snap ]] || { result FAIL "$vm copy" "no fidelity snapshot in primary (run the fidelity test first)"; continue; }
	c=
	for ((i = 0; i < 60; i++)); do
		if out=$(hfo snapshot list "$fqdn" --json 2>/dev/null); then
			c=$(js 'r = [s for s in j or [] if s.get("original") == sys.argv[1] and s.get("made_by_holdfast") and not s.get("gone_at")]
print(r[0]["id"] if r else "")' "$snap" <<<"$out")
			[[ -z $c ]] || break
		fi
		sleep 10
	done
	[[ -n $c ]] || { result FAIL "$vm copy" "no copy of ${snap:0:8} off-site within 10 minutes"; continue; }
	copy[$vm]=$c
	n=0
	for ((i = 0; i < 30; i++)); do
		n=$(hf agent show "$fqdn" --json | js 'print((j.get("offsite") or {}).get("copies", 0))')
		((n > 0)) && break
		sleep 5
	done
	if ((n > 0)); then
		result PASS "$vm copy" "${snap:0:8} copied off-site as ${c:0:8}; the host's page shows $n copies"
	else
		result FAIL "$vm copy" "${snap:0:8} copied as ${c:0:8}, but the host's page shows no copies"
	fi
done

# 2. and 3. A restore from off-site, on one VM.
vm=$restore_vm fqdn=$restore_vm.$HF_DOMAIN
if [[ -z ${copy[$vm]:-} ]]; then
	result FAIL "$vm restore from off-site" "no copy to restore"
	finish
fi
server_url=https://$HF_SERVER_VM.$HF_DOMAIN:8443
code=$(vm_ssh "$vm" "curl -s -o /dev/null -w '%{http_code}' --cacert /var/lib/holdfast-agent/server-ca.pem --cert /var/lib/holdfast-agent/agent.pem \
	$server_url/gateway/v1/offsite/$fqdn/config" </dev/null || true)
if [[ $code == 403 ]]; then
	result PASS "$vm off-site, no restore" "the host's certificate cannot read its off-site copy outside a restore (403)"
else
	result FAIL "$vm off-site, no restore" "the gateway answered $code to the host's read of its off-site copy, not 403"
fi
say "$vm: holdfast restore $fqdn ${copy[$vm]:0:8} --from-offsite --wait"
rc=0
out=$(vm_ssh "$HF_SERVER_VM" "timeout 1800 holdfast restore $fqdn ${copy[$vm]:0:12} --from-offsite --wait --json -m 'lab: from off-site'" </dev/null) || rc=$?
echo "the restore's event (exit $rc): $out"
if [[ $rc != 0 ]]; then
	result FAIL "$vm restore from off-site" "the restore failed: $out"
	finish
fi
dir=$(js 'print(j["detail"]["directory"])' <<<"$out")
rc=0
vm_ssh "$vm" "/usr/local/bin/hf-fidelity compare --json /srv/hf-fixture $dir/srv/hf-fixture >/root/hf-offsite-compare.json" </dev/null || rc=$?
cmp=$(vm_ssh "$vm" 'cat /root/hf-offsite-compare.json' </dev/null || true)
echo "$cmp"
kept=$(js 'print(sum(r["pass"] for r in j), len(j))' <<<"${cmp:-[]}" 2>/dev/null || echo "0 0")
if [[ $kept == "13 13" ]]; then
	result PASS "$vm restore from off-site" "restored from off-site through the gateway into $dir: 13 of 13 properties kept"
else
	result FAIL "$vm restore from off-site" "${kept% *} of 13 properties kept (exit $rc)"
fi
finish
