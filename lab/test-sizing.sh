#!/usr/bin/env bash
# lab/test-sizing.sh <lab> [<vm>...]: what one server's gateway carries (decision 042). Backup traffic
# passes through the server, so install.md says how much one carried in the lab. On each agent VM, or each one named:
#   1. a fixture of HF_SIZING_MIB (default 256) MiB of new random data in /srv/hf-sizing, which restic cannot
#      deduplicate or compress, and a job "sizing" for it;
#   2. every agent's backup of it started at once (holdfast job run), and timed by the server's events, from the first
#      start to the last finish; meanwhile the server's CPU (/proc/stat) and what its network received (/proc/net/dev);
#   3. the job removed again (its snapshots stay, as removing a job never deletes data) and the fixture deleted.
# The lab is one host's KVM: the figure is the lab's, a floor for real hardware. The results are in
# <state>/test-sizing.txt. Exit 0 when every backup succeeded, 1 otherwise, 2 misused, 3 the lab is not up.
set -euo pipefail
cd "$(dirname "$0")/.."
lab=${1:-}
[[ $lab =~ ^[a-z0-9]+$ ]] || { sed -n '2,/^set -e/{/^set -e/d; s/^# \{0,1\}//; p}' "$0" >&2; exit 2; }
shift
state=${XDG_STATE_HOME:-$HOME/.local/state}/holdfast/labs/$lab
[[ -f $state/lab.env ]] || { echo "test-sizing: the lab $lab is not up (lab/lab up $lab)" >&2; exit 3; }
# shellcheck source=/dev/null
source "$state/lab.env"
vms=("$@")
((${#vms[@]})) || vms=("${HF_AGENTS[@]}")
mib=${HF_SIZING_MIB:-256}
[[ $mib =~ ^[1-9][0-9]{0,4}$ ]] || { echo "test-sizing: HF_SIZING_MIB is $mib, not a size in MiB" >&2; exit 2; }
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
# event <fqdn> <command id>: the run's own event, as JSON, or nothing while it runs.
event() { hf host events "$1" --json | js 'r = [e for e in j or [] if e.get("command_id") == sys.argv[1]]
print(json.dumps(r[0]) if r else "")' "$2"; }
counters() { vm_ssh "$HF_SERVER_VM" "head -1 /proc/stat; awk '/eth0:/ {print \"rx\", \$2}' /proc/net/dev" </dev/null; }

# runall: every agent's sizing backup started at once; cmd[vm] is each one's command.
declare -A cmd
runall() {
	local tmp vm
	tmp=$(mktemp -d)
	for vm in "${vms[@]}"; do hf job run "$vm.$HF_DOMAIN" sizing --json >"$tmp/$vm" & done
	wait
	for vm in "${vms[@]}"; do cmd[$vm]=$(js 'print(j["command"])' <"$tmp/$vm"); done
	rm -rf "$tmp"
}
# waitrun <vm> <seconds>: the run's event, once it has one.
waitrun() {
	local ev= i
	for ((i = 0; i < $2 / 5; i++)); do
		ev=$(event "$1.$HF_DOMAIN" "${cmd[$1]}")
		[[ -n $ev ]] && break
		sleep 5
	done
	echo "$ev"
}

# The job first, on a tiny fixture: a new job's first backup runs at once, and must not be the one measured.
say "the job sizing on ${vms[*]}, backed up once with a tiny fixture"
for vm in "${vms[@]}"; do
	vm_ssh "$vm" 'rm -rf /srv/hf-sizing && mkdir -p /srv/hf-sizing && echo sizing >/srv/hf-sizing/README' </dev/null
	hf job set "$vm.$HF_DOMAIN" sizing --path /srv/hf-sizing --daily 05:00 --keep-daily 14d --keep-monthly 12mo -m "lab: the sizing test" >/dev/null
done
for vm in "${vms[@]}"; do
	for ((i = 0; i < 60; i++)); do
		[[ $(hf host show "$vm.$HF_DOMAIN" --json | js 'print(j.get("in_sync"))') == True ]] && break
		sleep 2
	done
done
runall
for vm in "${vms[@]}"; do waitrun "$vm" 300 >/dev/null; done

say "the fixture: $mib MiB of new random data in /srv/hf-sizing on each"
for vm in "${vms[@]}"; do
	vm_ssh "$vm" "head -c ${mib}M /dev/urandom >/srv/hf-sizing/random.bin" </dev/null
done
before=$(counters)
say "every agent's backup of it started at once"
runall
ok=1
all=()
for vm in "${vms[@]}"; do
	ev=$(waitrun "$vm" 1800)
	if [[ $(js 'print((j or {}).get("status", ""))' <<<"${ev:-null}") != success ]]; then
		say "  $vm: the sizing backup did not succeed within 30 minutes: ${ev:-no event}"
		ok=0
		continue
	fi
	# The window and the bytes, from the server's own events.
	all+=("$(js 'print(json.dumps({"vm": sys.argv[1], "started": j["started"], "finished": j["finished"], "bytes": j.get("bytes_added", 0)}))' "$vm" <<<"$ev")")
done
after=$(counters)
((${#all[@]})) || { echo "test-sizing: no backup succeeded" >&3; exit 1; }
summary=$(printf '%s\n' "${all[@]}" | python3 -I -c '
import json, sys
from datetime import datetime
def t(s):
    return datetime.fromisoformat(s.replace("Z", "+00:00"))
rows = [json.loads(l) for l in sys.stdin if l.strip()]
before, after = sys.argv[1].split("\n"), sys.argv[2].split("\n")
def cpu(lines):
    f = [int(x) for x in lines[0].split()[1:]]
    return sum(f), f[3] + f[4]
def rx(lines):
    return int(lines[1].split()[1])
(t0, i0), (t1, i1) = cpu(before), cpu(after)
busy = 100 * (1 - (i1 - i0) / max(t1 - t0, 1))
start, end = min(t(r["started"]) for r in rows), max(t(r["finished"]) for r in rows)
secs = max((end - start).total_seconds(), 0.001)
total = sum(r["bytes"] for r in rows)
for r in rows:
    s = (t(r["finished"]) - t(r["started"])).total_seconds()
    print("%-6s %6.0f MiB added in %5.1f s" % (r["vm"], r["bytes"] / 2**20, s))
print()
print("%d agents at once: %.0f MiB added in %.1f s through the gateway, %.1f MiB/s; the server: CPU %.0f%% busy, received %.0f MiB" %
      (len(rows), total / 2**20, secs, total / 2**20 / secs, busy, (rx(after) - rx(before)) / 2**20))
' "$before" "$after")
nproc=$(vm_ssh "$HF_SERVER_VM" nproc </dev/null)
mem=$(vm_ssh "$HF_SERVER_VM" "free -m | awk '/Mem:/ {print \$2}'" </dev/null)

say "the job sizing removed (its snapshots stay), and the fixture deleted"
for vm in "${vms[@]}"; do
	hf job remove "$vm.$HF_DOMAIN" sizing -m "lab: the sizing test is done" >/dev/null || true
	vm_ssh "$vm" 'rm -rf /srv/hf-sizing' </dev/null || true
done
{
	echo "The sizing test, $(date -u +%FT%TZ): Holdfast $version; the server $HF_SERVER_VM with $nproc vCPU and $mem MiB"
	echo
	echo "$summary"
	echo
	if ((ok)); then echo "result: PASS"; else echo "result: FAIL, a backup did not finish"; fi
} | tee "$HF_STATE/test-sizing.txt" >&3
((ok))
