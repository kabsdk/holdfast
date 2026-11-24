#!/usr/bin/env bash
# lab/test-recovery.sh <lab> [<vm>...]: M6's lab test. lab/lab test runs it after the fidelity test,
# which leaves each agent enrolled and its fixture backed up.
#   The bare-metal runbook's first path (docs/runbooks/bare-metal.md), on HF_REBUILD_VM (default: the first agent
#   named or of the lab): the fidelity snapshot noted; the fixture moved aside to /root/hf-fixture.orig; the agent's
#   packages removed, and /etc/holdfast and /var/lib/holdfast-agent deleted; the packages installed again from this
#   build; a new code (holdfast agent enroll-code); the agent enrolled with restore.allow_in_place; the noted snapshot
#   restored in place; hf-fidelity compare of the restored fixture with the one moved aside. allow_in_place is set back
#   to false afterwards.
# M6's dump test went with dump jobs in 0.6.0 (decision 052).
# The results are in <state>/test-recovery.txt. Exit 0 when everything passed, 1 otherwise, 2 misused, 3 the lab is not up.
# Enrollment codes travel on SSH's stdin only.
set -euo pipefail
cd "$(dirname "$0")/.."
lab=${1:-}
[[ $lab =~ ^[a-z0-9]+$ ]] || { sed -n '2,/^set -e/{/^set -e/d; s/^# \{0,1\}//; p}' "$0" >&2; exit 2; }
shift
state=${XDG_STATE_HOME:-$HOME/.local/state}/holdfast/labs/$lab
[[ -f $state/lab.env ]] || { echo "test-recovery: the lab $lab is not up (lab/lab up $lab)" >&2; exit 3; }
# shellcheck source=/dev/null
source "$state/lab.env"
vms=("$@")
((${#vms[@]})) || vms=("${HF_AGENTS[@]}")
rebuild=${HF_REBUILD_VM:-${vms[0]}}
server_url=https://$HF_SERVER_VM.$HF_DOMAIN:8443
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
js() { python3 -I -c "import json, sys
j = json.load(sys.stdin)
$1" "${@:2}"; }
results=()
result() { results+=("$(printf '%-6s %-44s %s' "$1" "$2" "$3")"); say "  $1: $2: $3"; }

# The bare-metal runbook's first path.
rebuild_vm() {
	local vm=$1 fqdn=$1.$HF_DOMAIN out snap code dir rc
	say "$vm: the bare-metal runbook's first path (docs/runbooks/bare-metal.md)"
	out=$(hf snapshot list "$fqdn" --json) || { result FAIL "$vm rebuild" "holdfast snapshot list failed"; return 1; }
	snap=$(js 'r = [s for s in j or [] if s.get("job_id") == "fidelity" and not s.get("gone_at")]
print(r[0]["short_id"] if r else "")' <<<"$out")
	[[ -n $snap ]] || { result FAIL "$vm rebuild" "no fidelity snapshot to restore (run the fidelity test first)"; return 1; }
	say "  $vm: the last good snapshot is $snap; the host loses its agent, its config and the fixture"
	vm_ssh "$vm" bash -s -- "$version" <<'EOF' || { result FAIL "$vm rebuild" "wiping or reinstalling the agent failed"; return 1; }
set -eu
v=$1 d=/var/tmp/holdfast-packages
systemctl disable -q --now holdfast-agent || true
if command -v dpkg >/dev/null; then dpkg -r holdfast-agent holdfast-restic >/dev/null; else rpm -e holdfast-agent holdfast-restic; fi
rm -rf /etc/holdfast /var/lib/holdfast-agent /root/hf-fixture.orig /var/restore/*
mv /srv/hf-fixture /root/hf-fixture.orig
test ! -e /usr/bin/holdfast-agent
if command -v dpkg >/dev/null; then dpkg -i "$d/holdfast-restic_${v}_amd64.deb" "$d/holdfast-agent_${v}_amd64.deb" >/dev/null
else rpm -U "$d/holdfast-restic-$v.x86_64.rpm" "$d/holdfast-agent-$v.x86_64.rpm"; fi
echo "installed: $(holdfast-agent version)"
EOF
	out=$(hf agent enroll-code "$fqdn" --json) || { result FAIL "$vm rebuild" "holdfast agent enroll-code failed"; return 1; }
	code=$(js 'print(j["code"])' <<<"$out")
	if ! vm_ssh "$vm" "read -r code && holdfast-agent enroll --server $server_url --name $fqdn --ca-sha256 $pin --code \"\$code\" &&
		sed -i 's/^\( *\)allow_in_place: false/\1allow_in_place: true/' /etc/holdfast/agent.yaml && grep -q 'allow_in_place: true' /etc/holdfast/agent.yaml &&
		systemctl enable -q --now holdfast-agent" <<<"$code"; then
		code=
		result FAIL "$vm rebuild" "enrolling again failed"
		return 1
	fi
	code=
	for ((i = 0; i < 60; i++)); do
		out=$(hf agent show "$fqdn" --json 2>/dev/null) && [[ $(js 'a = j.get("agent") or {}
print(j["state"] == "enrolled" and (a.get("restore_bounds") or {}).get("allow_in_place") is True)' <<<"$out") == True ]] && break
		sleep 2
	done
	say "  $vm: enrolled again; holdfast restore $fqdn $snap --in-place --wait"
	rc=0
	out=$(vm_ssh "$HF_SERVER_VM" "timeout 1800 holdfast restore $fqdn $snap --in-place --wait --json -m 'lab: the bare-metal runbook'" </dev/null) || rc=$?
	echo "the restore's event (exit $rc): $out"
	[[ $rc == 0 ]] || { result FAIL "$vm rebuild" "the in-place restore failed: $out"; return 1; }
	rc=0
	vm_ssh "$vm" "/usr/local/bin/hf-fidelity compare --json /root/hf-fixture.orig /srv/hf-fixture >/root/hf-rebuild-compare.json" </dev/null || rc=$?
	out=$(vm_ssh "$vm" 'cat /root/hf-rebuild-compare.json' </dev/null || true)
	echo "$out"
	local kept
	kept=$(js 'print(sum(r["pass"] for r in j), len(j))' <<<"${out:-[]}" 2>/dev/null || echo "0 0")
	vm_ssh "$vm" "sed -i 's/^\( *\)allow_in_place: true/\1allow_in_place: false/' /etc/holdfast/agent.yaml && systemctl restart holdfast-agent && rm -rf /root/hf-fixture.orig" </dev/null || true
	if [[ $kept == "13 13" ]]; then
		result PASS "$vm rebuild" "wiped, reinstalled, enrolled again, restored in place: 13 of 13 properties kept"
	else
		result FAIL "$vm rebuild" "after the in-place restore: ${kept% *} of 13 properties kept (exit $rc)"
		return 1
	fi
}

rebuild_vm "$rebuild" || true

{
	echo "M6's lab test, $(date -u +%FT%TZ): Holdfast $version"
	echo
	printf '%s\n' "${results[@]}"
	echo
	fails=$(printf '%s\n' "${results[@]}" | grep -c '^FAIL' || true)
	if ((fails == 0)); then echo "result: PASS, ${#results[@]} of ${#results[@]}"; else echo "result: FAIL, $fails of ${#results[@]} failed"; fi
} | tee "$HF_STATE/test-recovery.txt" >&3
! grep -q '^FAIL' <<<"$(printf '%s\n' "${results[@]}")"
