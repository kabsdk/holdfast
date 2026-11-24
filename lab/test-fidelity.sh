#!/usr/bin/env bash
# lab/test-fidelity.sh <lab> [<vm>...]: M2's "done when": a backup and a restore through Holdfast keep
# all 13 metadata properties on each agent VM, against the lab's RGW. lab/lab test <lab> runs it, holding the lab's lock,
# after building and deploying this checkout and provisioning the server (site.yml --tags binaries,hfserver). For each
# agent VM, or each one named:
#   0. (once) lab/rgw-documented-layout.sh --keep-data, which gives holdfast-maint and holdfast-gateway docs/storage.md's
#      statements in the bucket policy, keeping the bucket's data, and lab/rgw-offsite-layout.sh, the off-site bucket.
#      No host has an RGW user: hosts back up through the server's gateway (decision 041)
#   1. the fixture, made anew: hf-fidelity build /srv/hf-fixture
#   2. Holdfast, on the server: holdfast agent add <fqdn>, then holdfast job set <fqdn> fidelity --path /srv/hf-fixture --daily 03:00 --keep-daily 14d --keep-monthly 12mo;
#      on the VM: holdfast-agent enroll against https://<server>:8443 with the lab CA's pin, which makes the agent's own
#      key and certificate, restore.root /var/restore in agent.yaml, and holdfast-agent.service enabled and started. The
#      first backup runs at once. A host Holdfast knows whose agent is enrolled to this server with a certificate:
#      holdfast job run <fqdn> fidelity instead. A host Holdfast knows whose agent is not (an agent from before M8 has
#      a token, not a certificate), or whose repository is gone (restic exit 10: the bucket was made again): a new code
#      (holdfast agent enroll-code), and the agent enrolled afresh, which makes Holdfast initialise the repository.
#   3. the backup's event (holdfast agent events --json), the job's newest snapshot (holdfast snapshot list --refresh --json),
#      a restore of it through the agent into a new directory (holdfast restore --wait --json), then on the VM
#      hf-fidelity compare /srv/hf-fixture <directory>/srv/hf-fixture. The restores of earlier runs are removed first.
#   4. on SELinux VMs, the denials (AVC, SELINUX_ERR) logged since the VM's run began; on a failure, the agent's and the
#      server's journal too
# Before the VMs, the login check (lab/test-login.sh): Authelia's discovery, the UI's 401 without a session and its
# redirect to Authelia, and ada's login through Authelia without a browser, then her logout. Then each VM's OS, kernel
# and SELinux mode, the 13 properties and the login checks, PASS or FAIL, on the terminal and in
# <state>/test-fidelity.txt and test-fidelity.json. Exit 0 when all 13 pass on every VM and every login check passes, 1
# when one does not or a step failed, 2 misused, 3 the lab is not up. Enrollment codes travel on SSH's stdin only, and
# the login's password on curl's: never in an argument on this host, in the log or on the terminal.
set -euo pipefail
cd "$(dirname "$0")/.."
lab=${1:-}
[[ $lab =~ ^[a-z0-9]+$ ]] || { sed -n '2,/^set -e/{/^set -e/d; s/^# \{0,1\}//; p}' "$0" >&2; exit 2; }
shift
state=${XDG_STATE_HOME:-$HOME/.local/state}/holdfast/labs/$lab
[[ -f $state/lab.env ]] || { echo "test-fidelity: the lab $lab is not up (lab/lab up $lab)" >&2; exit 3; }
# shellcheck source=/dev/null
source "$state/lab.env"
vms=("$@")
((${#vms[@]})) || vms=("${HF_AGENTS[@]}")
for vm in "${vms[@]}"; do
	[[ " ${HF_AGENTS[*]} " == *" $vm "* ]] || { echo "test-fidelity: '$vm' is not an agent of the lab $lab (${HF_AGENTS[*]})" >&2; exit 2; }
done
server_url=https://$HF_SERVER_VM.$HF_DOMAIN:8443
pin_file=$HF_STATE/ca/ca.sha256
[[ -s $pin_file ]] || { echo "test-fidelity: no $pin_file: the server is not provisioned (lab/lab test $lab does it, or lab/lab up $lab)" >&2; exit 3; }
pin=$(cat "$pin_file")
restic=/usr/libexec/holdfast/restic
fixture=/srv/hf-fixture
job=fidelity
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# A line for the terminal (fd 3, which lab/lab opens; stdout when run on its own) and the log alike.
{ true >&3; } 2>/dev/null || exec 3>&1
say() { echo "== $*"; echo "lab: $*" >&3; }

vm_ssh() {
	local vm=$1 ip
	shift
	ip=HF_VM_IP_$vm
	ssh -i "$HF_SSH_KEY" -o IdentitiesOnly=yes -o "UserKnownHostsFile=$HF_STATE/known_hosts" -o StrictHostKeyChecking=accept-new \
		-o LogLevel=ERROR -o BatchMode=yes -o ConnectTimeout=10 "root@${!ip}" "$@"
}
# hf <arguments>: the CLI on the server, as root over its API socket (each argument quoted for the remote shell).
hf() { vm_ssh "$HF_SERVER_VM" "$(printf '%q ' holdfast "$@")" </dev/null; }
# js <python code> [args]: a python snippet over stdin's JSON (j), for picking fields out of the CLI's --json answers.
js() { python3 -I -c "import json, sys
j = json.load(sys.stdin)
$1" "${@:2}"; }

say "Holdfast's server: $server_url, the API as root on $HF_SERVER_VM"
if ! out=$(hf status); then
	say "holdfast status failed on $HF_SERVER_VM: the log has why (lab/lab ssh $lab $HF_SERVER_VM journalctl -u holdfast-server)"
	exit 1
fi
echo "$out"

# Login on the UI, through Authelia: its checks are in the summary.
bash lab/test-login.sh "$lab" "$work/login.tsv" || true

# 0. The bucket policy for the server's two keys, and the off-site bucket. No host has an RGW user (decision 041).
say "the bucket policy and the off-site bucket (lab/rgw-documented-layout.sh --keep-data, lab/rgw-offsite-layout.sh on $HF_STORAGE_VM)"
if ! vm_ssh "$HF_STORAGE_VM" 'bash -s -- --keep-data' <lab/rgw-documented-layout.sh; then
	say "lab/rgw-documented-layout.sh --keep-data failed on $HF_STORAGE_VM: the log has why"
	exit 1
fi
if ! vm_ssh "$HF_STORAGE_VM" 'bash -s' <lab/rgw-offsite-layout.sh; then
	say "lab/rgw-offsite-layout.sh failed on $HF_STORAGE_VM: the log has why"
	exit 1
fi
# The keys of before M8 in the lab's state, whose users the two scripts removed: each agent's, and M7's vault's.
rm -f "$HF_STATE"/credentials/{host,hfb}-*.env "$HF_STATE"/credentials/holdfast-vault-{read,maint}.env

run_vm() {
	local vm=$1 fqdn=$1.$HF_DOMAIN res=$work/$1 out rc t0 hosts hstate enrolled ids0 mode cmd ev status snap newest dir i
	mkdir -p "$res"
	fail() {
		echo "FAIL  $vm: $*"
		printf '%s\n' "$*" >"$res/error"
		say "  $vm: FAILED: $*"
		if [[ -n ${t0:-} ]]; then
			echo "--- journalctl -u holdfast-agent on $vm since its run began"
			vm_ssh "$vm" "journalctl -u holdfast-agent --since @$t0 --no-pager -o short-iso | tail -80" </dev/null || true
			echo "--- journalctl -u holdfast-server on $HF_SERVER_VM since then"
			vm_ssh "$HF_SERVER_VM" "journalctl -u holdfast-server --since @$t0 --no-pager -o short-iso | tail -80" </dev/null || true
			denials
		fi
		return 1
	}
	# The SELinux denials logged on the VM since its run began (ausearch takes its start in the local format, %x %T).
	denials() {
		grep -qx 'selinux=none' "$res/facts" && return 0
		vm_ssh "$vm" "ausearch -i -m AVC,USER_AVC,SELINUX_ERR,USER_SELINUX_ERR -ts \$(date -d @$t0 +%x) \$(date -d @$t0 +%T) 2>&1 || true" \
			</dev/null >"$res/avc" || true
		echo "--- ausearch -m AVC,USER_AVC,SELINUX_ERR,USER_SELINUX_ERR on $vm since its run began"
		cat "$res/avc"
	}

	if ! out=$(vm_ssh "$vm" '. /etc/os-release; echo "os=$PRETTY_NAME"; echo "kernel=$(uname -r)"
		echo "selinux=$(getenforce 2>/dev/null || echo none)"; echo "t0=$(date +%s)"' </dev/null); then
		fail "$vm does not answer SSH"
		return 1
	fi
	printf '%s\n' "$out" >"$res/facts"
	t0=$(sed -n 's/^t0=//p' <<<"$out")
	say "$vm ($fqdn): $(sed -n 's/^os=//p' <<<"$out"), kernel $(sed -n 's/^kernel=//p' <<<"$out"), SELinux $(sed -n 's/^selinux=//p' <<<"$out")"

	# 1. The fixture, made anew.
	say "  $vm: hf-fidelity build $fixture"
	vm_ssh "$vm" "set -e; rm -rf $fixture; /usr/local/bin/hf-fidelity build $fixture; ls -labn --time-style=full-iso $fixture" </dev/null ||
		{ fail "hf-fidelity build $fixture failed"; return 1; }

	# 2. The host in Holdfast, and its agent.
	hosts=$(hf agent list --json) || { fail "holdfast agent list failed"; return 1; }
	hstate=$(js 'h = [h for h in j or [] if h["fqdn"] == sys.argv[1]]
print(h[0]["state"] if h else "absent")' "$fqdn" <<<"$hosts")
	enrolled=$(vm_ssh "$vm" "f=/etc/holdfast/agent.yaml; test -s /var/lib/holdfast-agent/agent.pem &&
		grep -qE '^fqdn: \"?$fqdn\"?\$' \$f && grep -qE '^server: \"?$server_url\"?\$' \$f && grep -qE '^ca_sha256: \"?$pin\"?\$' \$f &&
		echo yes || echo no" </dev/null)
	echo "Holdfast's state of $fqdn: $hstate; its agent enrolled to $server_url with the lab's pin: $enrolled"
	ids0=
	if [[ $hstate != absent ]]; then
		out=$(hf agent events "$fqdn" --json) || { fail "holdfast agent events failed"; return 1; }
		ids0=$(js 'print(" ".join(e["id"] for e in j or []))' <<<"$out")
		out=$(hf agent show "$fqdn" --json) || { fail "holdfast agent show failed"; return 1; }
		if [[ $(js 'print(" ".join(next((x["paths"] for x in j.get("jobs") or [] if x["id"] == sys.argv[1]), ["-"])))' "$job" <<<"$out") != "$fixture" ]]; then
			say "  $vm: holdfast job set $fqdn $job --path $fixture --daily 03:00 --keep-daily 14d --keep-monthly 12mo"
			hf job set "$fqdn" "$job" --path "$fixture" --daily 03:00 --keep-daily 14d --keep-monthly 12mo -m "lab/lab test: the fidelity fixture" || { fail "holdfast job set failed"; return 1; }
		fi
	fi
	mode=enroll
	if [[ $hstate != absent && $hstate != awaiting_enrollment && $enrolled == yes ]]; then
		mode=run
		# A repository that is gone (the bucket made again, without --keep-data): enrolling again makes Holdfast
		# initialise it (ensureStorage). Any other failure to read it is a failure.
		if ! out=$(hf snapshot list "$fqdn" --refresh --json 2>&1 >/dev/null); then
			echo "holdfast snapshot list $fqdn --refresh: $out"
			grep -qE 'restic exit 10|repository does not exist' <<<"$out" || { fail "holdfast snapshot list --refresh failed: $out"; return 1; }
			say "  $vm: the repository of $fqdn is gone (restic exit 10): the agent enrolls again, and Holdfast initialises the repository"
			mode=enroll
		fi
	fi
	if [[ $mode == enroll ]]; then
		# What the agent has belongs to no enrollment this server knows: it starts afresh.
		vm_ssh "$vm" 'systemctl disable -q --now holdfast-agent 2>/dev/null; rm -rf /etc/holdfast/agent.yaml /etc/holdfast/credentials /var/lib/holdfast-agent' </dev/null
		local code
		if [[ $hstate == absent ]]; then
			say "  $vm: holdfast agent add $fqdn; holdfast job set $fqdn $job --path $fixture --daily 03:00 --keep-daily 14d --keep-monthly 12mo"
			out=$(vm_ssh "$HF_SERVER_VM" "holdfast agent add $fqdn -m 'lab/lab test: the fidelity fixture' --json" \
				</dev/null) || { fail "holdfast agent add failed (the log has its answer)"; return 1; }
			code=$(js 'print(j["enrollment"]["code"])' <<<"$out")
			js 'print("added in revision %s; storage checks: %s%s" % (j["revision"], ", ".join(f["code"] for f in (j.get("storage") or {}).get("findings") or []) or "all passed", "; warning: " + j["warning"] if j.get("warning") else ""))' <<<"$out"
			vm_ssh "$HF_SERVER_VM" "holdfast job set $fqdn $job --path $fixture --daily 03:00 --keep-daily 14d --keep-monthly 12mo -m 'lab/lab test: the fidelity fixture' --json" \
				</dev/null >/dev/null || { fail "holdfast job set failed (the log has its answer)"; return 1; }
		else
			say "  $vm: Holdfast has $fqdn ($hstate); a new enrollment code: holdfast agent enroll-code $fqdn"
			out=$(hf agent enroll-code "$fqdn" --json) || { fail "holdfast agent enroll-code failed"; return 1; }
			code=$(js 'print(j["code"])' <<<"$out")
		fi
		out=
		say "  $vm: holdfast-agent enroll --server $server_url --name $fqdn --ca-sha256 $pin --restic $restic --code (on stdin)"
		if ! vm_ssh "$vm" "read -r code && holdfast-agent enroll --server $server_url --name $fqdn --ca-sha256 $pin --restic $restic --code \"\$code\"" <<<"$code"; then
			code=
			fail "holdfast-agent enroll failed (the log has its error)"
			return 1
		fi
		code=
	fi
	# The host's own restore bounds (decision 030): new directories under /var/restore. Then the service.
	vm_ssh "$vm" bash -s <<'EOF' || { fail "setting agent.yaml's restore.root or starting holdfast-agent failed"; return 1; }
set -eu
f=/etc/holdfast/agent.yaml
root() { awk '/^restore:/ {r = 1; next} r && /^[^ ]/ {r = 0} r && $1 == "root:" {print $2}' "$f"; }
changed=0
if [ "$(root)" != /var/restore ]; then
	if [ -n "$(root)" ]; then
		sed -i '/^restore:/,/^[^ ]/ s|^\( \{1,\}\)root:.*|\1root: /var/restore|' "$f"
	elif grep -q '^restore:' "$f"; then
		sed -i 's|^restore:.*|restore:\n    root: /var/restore|' "$f"
	else
		printf 'restore:\n    root: /var/restore\n' >>"$f"
	fi
	changed=1
fi
echo "agent.yaml (changed: $changed):"
sed 's/^/    /' "$f"
systemctl enable -q holdfast-agent
if [ "$changed" = 1 ]; then systemctl restart holdfast-agent; else systemctl start holdfast-agent; fi
echo "holdfast-agent: $(systemctl is-active holdfast-agent)"
EOF

	# 3. The backup, its snapshot, the restore and the comparison.
	cmd=
	if [[ $mode == run ]]; then
		out=$(hf job run "$fqdn" "$job" --json) || { fail "holdfast job run failed"; return 1; }
		cmd=$(js 'print(j["command"])' <<<"$out")
		say "  $vm: holdfast job run $fqdn $job (command $cmd); waiting for its backup"
	else
		say "  $vm: waiting for the first backup, which the agent runs at once after enrolling"
	fi
	ev=
	for ((i = 0; i < 180; i++)); do
		if out=$(hf agent events "$fqdn" --json 2>/dev/null); then
			ev=$(js 'ids0, cmd, job = set(sys.argv[1].split()), sys.argv[2], sys.argv[3]
for e in j or []:
    if e.get("kind") == "backup" and e.get("job_id") == job and e["id"] not in ids0 and e.get("status") != "skipped" \
            and (not cmd or e.get("command_id") == cmd):
        print(json.dumps(e))
        break' "$ids0" "$cmd" "$job" <<<"$out")
		fi
		[[ -z $ev ]] || break
		sleep 5
	done
	[[ -n $ev ]] || { fail "no backup of $job within 15 minutes (holdfast agent events $fqdn)"; return 1; }
	printf '%s\n' "$ev" >"$res/backup.json"
	echo "the backup's event: $ev"
	status=$(js 'print(j.get("status", ""))' <<<"$ev")
	snap=$(js 'print(j.get("snapshot_id", ""))' <<<"$ev")
	[[ $status == success && -n $snap ]] || { fail "the backup's status is $status: $(js 'print(j.get("error", ""))' <<<"$ev")"; return 1; }
	say "  $vm: backup $status: snapshot ${snap:0:8}, $(js 'print(j.get("total_files", 0))' <<<"$ev") files"

	out=$(hf snapshot list "$fqdn" --refresh --json) || { fail "holdfast snapshot list --refresh failed"; return 1; }
	echo "holdfast snapshot list $fqdn --refresh --json:"
	echo "$out"
	# Newest first, as the API lists them; the job's newest one still in the repository.
	newest=$(js 'r = [s for s in j or [] if s.get("job_id") == sys.argv[1] and not s.get("gone_at")]
print(r[0]["id"], r[0]["short_id"], str(r[0].get("made_by_holdfast")).lower()) if r else print("- - -")' "$job" <<<"$out")
	read -r newest short made <<<"$newest"
	[[ $newest == "$snap"* || $snap == "$newest"* ]] ||
		{ fail "the job's newest snapshot is ${newest:0:8}, not the backup's ${snap:0:8}"; return 1; }
	[[ $made == true ]] || { fail "the catalogue says snapshot $short was not made by Holdfast"; return 1; }

	vm_ssh "$vm" 'rm -rf /var/restore/*' </dev/null
	say "  $vm: holdfast restore $fqdn $short --wait --json"
	rc=0
	out=$(vm_ssh "$HF_SERVER_VM" "timeout 1800 holdfast restore $fqdn $short --wait --json" </dev/null) || rc=$?
	printf '%s\n' "$out" >"$res/restore.json"
	echo "the restore's event (exit $rc): $out"
	status=$(js 'print(j.get("status", ""))' <<<"${out:-null}" 2>/dev/null || true)
	dir=$(js 'print((j.get("detail") or {}).get("directory", ""))' <<<"${out:-null}" 2>/dev/null || true)
	[[ $rc == 0 && $status == success && $dir == /var/restore/* ]] ||
		{ fail "the restore did not succeed (exit $rc, status ${status:-none}): $(js 'print(j.get("error", ""))' <<<"${out:-null}" 2>/dev/null || true)"; return 1; }
	say "  $vm: restored into $dir; hf-fidelity compare $fixture $dir$fixture"

	rc=0
	vm_ssh "$vm" "ls -labn --time-style=full-iso $dir$fixture; /usr/local/bin/hf-fidelity compare --json $fixture $dir$fixture >/root/hf-fidelity-compare.json" </dev/null || rc=$?
	vm_ssh "$vm" 'cat /root/hf-fidelity-compare.json' </dev/null >"$res/compare.json" || true
	cat "$res/compare.json"
	((rc <= 1)) && js 'assert len(j) == 13' <"$res/compare.json" 2>/dev/null ||
		{ fail "hf-fidelity compare could not compare the trees (exit $rc)"; return 1; }
	denials
	say "  $vm: $(js 'print("%d of 13 properties kept" % sum(r["pass"] for r in j))' <"$res/compare.json")"
}

for vm in "${vms[@]}"; do
	run_vm "$vm" || true
done

# The table, on the terminal, in the log and in the lab's state.
set +e
python3 -I - "$work" "$HF_STATE" "$server_url" "$HF_RGW_URL" "$HF_BUCKET" "${HF_UI_URL:-}" "${HF_OIDC_ISSUER:-}" "${HF_AUTHELIA_VERSION:-}" "${vms[@]}" <<'EOF' | tee "$HF_STATE/test-fidelity.txt" >&3
import datetime, json, os, sys
work, state, server, rgw, bucket, ui, issuer, authelia, vms = *sys.argv[1:9], sys.argv[9:]
def read(vm, name, default=None):
    try:
        with open(os.path.join(work, vm, name)) as f:
            return f.read()
    except OSError:
        return default
out, ok = {}, True
for vm in vms:
    facts = dict(l.split("=", 1) for l in (read(vm, "facts") or "").splitlines() if "=" in l)
    try:
        props = json.loads(read(vm, "compare.json") or "null") or []
    except ValueError:
        props = []
    avc = read(vm, "avc")
    denials = None if avc is None else sum(1 for l in avc.splitlines() if l.startswith("type="))
    err = (read(vm, "error") or "").strip()
    out[vm] = {"os": facts.get("os", "?"), "kernel": facts.get("kernel", "?"), "selinux": facts.get("selinux", "?"),
               "denials": denials, "error": err or None, "properties": props}
    ok = ok and not err and len(props) == 13 and all(p["pass"] for p in props)
now = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
print(f"The fidelity test, {now}: backed up and restored through Holdfast ({server}), RGW {rgw}, bucket {bucket}")
w = max(len(vm) for vm in vms)
print()
print(f"{'VM':<{w}}  {'OS':<30}  {'kernel':<34}  {'SELinux':<10}  denials since the run began")
for vm, r in out.items():
    d = "-" if r["selinux"] in ("none", "?") or r["denials"] is None else str(r["denials"])
    print(f"{vm:<{w}}  {r['os']:<30}  {r['kernel']:<34}  {r['selinux']:<10}  {d}")
names = []
for r in out.values():
    for p in r["properties"]:
        if p["property"] not in names:
            names.append(p["property"])
if names:
    print()
    print(f"{'property':<50}" + "".join(f"  {vm:<6}" for vm in vms))
    for n in names:
        row = ""
        for vm in vms:
            p = next((p for p in out[vm]["properties"] if p["property"] == n), None)
            row += f"  {('-' if p is None else 'PASS' if p['pass'] else 'FAIL'):<6}"
        print(f"{n:<50}{row}")
fails = [(vm, p) for vm in vms for p in out[vm]["properties"] if not p["pass"]]
if fails:
    print()
    for vm, p in fails:
        print(f"FAIL {vm}: {p['property']}: {p['detail']}")
for vm in vms:
    if out[vm]["error"]:
        print(f"FAIL {vm}: {out[vm]['error']}")
    elif len(out[vm]["properties"]) != 13:
        print(f"FAIL {vm}: no comparison")
# The login checks (lab/test-login.sh): PASS or FAIL, the check, what was seen.
login = []
try:
    with open(os.path.join(work, "login.tsv")) as f:
        for l in f.read().splitlines():
            w = l.split("\t", 2)
            if len(w) == 3:
                login.append({"check": w[1], "pass": w[0] == "PASS", "seen": w[2]})
except OSError:
    pass
login_ok = bool(login) and all(c["pass"] for c in login)
print()
print(f"login on {ui or '?'} through Authelia {authelia or '?'} at {issuer or '?'}, from this host")
if login:
    for c in login:
        print(f"{c['check']:<68}  {'PASS' if c['pass'] else 'FAIL'}")
    for c in login:
        if not c["pass"]:
            print(f"FAIL login: {c['check']}: {c['seen']}")
else:
    print("FAIL login: no checks ran (lab/test-login.sh; the log has why)")
passed = sum(p["pass"] for r in out.values() for p in r["properties"])
print()
print(f"result: {'PASS' if ok and login_ok else 'FAIL'}, {passed} of {13 * len(vms)} properties kept on {len(vms)} VMs, "
      f"{sum(c['pass'] for c in login)} of {len(login)} login checks passed")
ok = ok and login_ok
with open(os.path.join(state, "test-fidelity.json"), "w") as f:
    json.dump({"at": now, "server": server, "rgw": rgw, "bucket": bucket, "pass": ok, "vms": out,
               "login": {"ui": ui, "issuer": issuer, "authelia": authelia, "pass": login_ok, "checks": login}}, f, indent=2)
sys.exit(0 if ok else 1)
EOF
rc=${PIPESTATUS[0]}
set -e
cat "$HF_STATE/test-fidelity.txt"
echo "lab: the results are in $HF_STATE/test-fidelity.txt and test-fidelity.json; everything else is in the log" >&3
exit "$rc"
