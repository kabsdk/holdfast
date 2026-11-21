#!/usr/bin/env bash
# lab/rgw-isolation.sh <lab> <user-policy|bucket-policy> [--maint-policy]: the RGW host-isolation experiment on a ready
# lab, which decides how Holdfast provisions a host's S3 identity (spec §2.3, decision 011, plan-m1 Q4; lab/README.md,
# The RGW experiment). lab/lab isolation <lab> <mechanism> [--maint-policy] runs it, holding the lab's lock, and keeps
# its transcript in the lab's state (isolation-<mechanism>[-maint-policy].txt).
#   1. As holdfast-provisioner (caps users=*;user-policy=*), through the Admin Ops API with SigV4-signed requests
#      (s3req, botocore's signer): test-host made anew with an access key and secret chosen here, kept in the lab's
#      state (credentials/test-host.env); then the same create again and the key call, and radosgw-admin's view.
#   2. Holdfast's host policy for test.example.com, by one mechanism:
#        user-policy    an IAM user policy on test-host (aws iam put-user-policy, as holdfast-provisioner)
#        bucket-policy  a bucket policy on the bucket naming arn:aws:iam:::user/test-host (as holdfast-maint, the owner)
#      Both allow s3:ListBucket with s3:prefix repos/<fqdn> or repos/<fqdn>/*, s3:GetObject and s3:PutObject on
#      repos/<fqdn>/*, and s3:DeleteObject on repos/<fqdn>/locks/* only. --maint-policy adds a bucket-policy statement
#      that gives holdfast-maint s3:* on the bucket and its objects (with user-policy, it is the bucket policy's only
#      statement). Then whether RGW takes S3 Object Ownership (BucketOwnerEnforced), on a scratch bucket.
#   3. As test-host: what the policy must allow and what it must refuse, each request's HTTP status and error code
#      (a refusal counts only as 403 AccessDenied); then whether holdfast-maint reads and deletes what test-host wrote.
#   4. From an agent: restic init as holdfast-maint, backup as test-host, forget as test-host (must fail: it would delete
#      a snapshot), check --read-data and forget --prune as holdfast-maint (the maintenance leader's work).
# Each step prints its command and PASS (as Holdfast wants), FAIL (not), or INFO (recorded, no expectation). It exits
# 0 when no step failed, 1 when one did, 2 misused, 3 when the lab is not up.
set -euo pipefail
cd "$(dirname "$0")/.."
lab=${1:-} mech=${2:-} maint=${3:-}
if [[ ! $lab =~ ^[a-z0-9]+$ || ($mech != user-policy && $mech != bucket-policy) || ($maint != "" && $maint != --maint-policy) ]] || (($# > 3)); then
	sed -n '2,/^set -e/{/^set -e/d; s/^# \{0,1\}//; p}' "$0" >&2
	exit 2
fi
state=${XDG_STATE_HOME:-$HOME/.local/state}/holdfast/labs/$lab
[[ -f $state/lab.env ]] || { echo "rgw-isolation: the lab $lab is not up (lab/lab up $lab)" >&2; exit 3; }
# shellcheck source=/dev/null
source "$state/lab.env"
creds=$HF_STATE/credentials
fqdn=test.example.com
python3 lab/ansible/files/credential.py s3 "$creds/test-host.env" HFTEST
python3 lab/ansible/files/credential.py password "$creds/test-host.restic.pw"
agent=${HF_AGENTS[0]}

vm_ssh() {
	local vm=$1 ip
	shift
	ip=HF_VM_IP_$vm
	ssh -i "$HF_SSH_KEY" -o IdentitiesOnly=yes -o "UserKnownHostsFile=$HF_STATE/known_hosts" -o StrictHostKeyChecking=accept-new \
		-o LogLevel=ERROR -o BatchMode=yes "root@${!ip}" "$@"
}
# put <vm> <file> <name>: the file into /root/hf-lab/<name> on the VM, 0600, through SSH's stdin.
put() { vm_ssh "$1" "umask 077 && mkdir -p /root/hf-lab && cat >/root/hf-lab/$3" <"$2"; }
put "$HF_STORAGE_VM" "$creds/test-host.env" test-host.env
for f in test-host.env holdfast-maint.env test-host.restic.pw; do put "$agent" "$creds/$f" "$f"; done

echo "# RGW isolation experiment: the lab $lab, mechanism $mech${maint:+ $maint}, $(date -u +%FT%TZ)"
echo "# RGW $HF_RGW_URL, bucket $HF_BUCKET, host $fqdn (prefix repos/$fqdn/), RGW user test-host"
sed 's/^/# /' "$HF_CEPH_VERSION_FILE"
out=$(mktemp) && trap 'rm -f "$out"' EXIT

# Steps 1-3, on the storage VM, where awscli and s3req are.
vm_ssh "$HF_STORAGE_VM" bash -s -- "$HF_RGW_URL" "$HF_BUCKET" "$HF_REGION" "$mech" "$fqdn" "${maint:-none}" <<'EOF' | tee "$out" || true
set -u
RGW=$1 B=$2 REGION=$3 MECH=$4 FQDN=$5 MAINT=$6
P=repos/$FQDN O=repos/other.example.com
cd /root/hf-lab
export AWS_DEFAULT_REGION=$REGION AWS_PAGER= PATH=$PATH:/snap/bin
# as <user> <command>...: the command with that user's key in its environment, never in argv.
as() { local u=$1; shift; ( set -a; . "/root/hf-lab/$u.env"; set +a; "$@" ); }
# awsd <aws arguments>...: aws with --debug, summed up as s3req sums up a request: METHOD PATH -> STATUS [Code],
# then aws's own output.
awsd() {
	local err rc line code
	err=$(mktemp)
	aws --endpoint-url "$RGW" --output json --debug "$@" >"$err.out" 2>"$err"; rc=$?
	line=$(grep -oE '"(GET|PUT|POST|DELETE|HEAD) [^ ]* HTTP/1\.1" [0-9]{3}' "$err" | tail -1)
	code=$(grep -hoE 'An error occurred \([A-Za-z]+\)' "$err" | tail -1 | sed 's/.*(\(.*\))/\1/')
	if [[ -n $line ]]; then
		read -r m p _ st <<<"${line//\"/}"
		echo "$m $p -> $st${code:+ $code}"
	else
		echo "aws exited $rc without a request${code:+: $code}"
	fi
	sed 's/^/    /' "$err.out"
	grep -m1 -E 'An error occurred' "$err" | sed 's/^/    /'
	rm -f "$err" "$err.out"
	return $rc
}
# check <want> <user> <command>...: runs the command as the user and prints it, its result, and PASS when the HTTP
# status is <want> (a status, or 2xx; 403 counts only with AccessDenied, not with a signature error), FAIL when it is
# not, INFO when <want> is "-".
check() {
	local want=$1 who=$2 res st code
	shift 2
	echo "\$ ($who) $*" | sed 's/secret-key=[^&]*/secret-key=***/g'
	res=$(as "$who" "$@" 2>&1)
	sed 's/^/    /' <<<"$res"
	read -r st code < <(sed -n 's/^[A-Z]* .* -> \([0-9][0-9][0-9]\) *\([A-Za-z]*\).*/\1 \2/p' <<<"$res" | head -1)
	if [[ $want == - ]]; then
		echo "INFO  $who: ${st:-no status}${code:+ $code}"
	elif [[ ($want == 403 && $st == 403 && $code == AccessDenied) || ($want != 403 && $st == "$want") || ($want == 2xx && $st == 2??) ]]; then
		echo "PASS  $who: $st${code:+ $code}, as wanted ($want)"
	else
		echo "FAIL  $who: ${st:-no status}${code:+ $code}, wanted $want$([[ $want != 403 ]] || echo ' AccessDenied')"
	fi
}
# s3py <user> <python code> [args]: a botocore S3 client c on RGW, as the user.
s3py() {
	local who=$1 code=$2
	shift 2
	as "$who" python3 -I -c 'import sys, botocore.session
from botocore.config import Config
c = botocore.session.get_session().create_client("s3", endpoint_url=sys.argv[1], config=Config(s3={"addressing_style": "path"}))
'"$code" "$RGW" "$B" "$@"
}
TH_KEY=$(sed -n 's/^AWS_ACCESS_KEY_ID=//p' test-host.env)
TH_SECRET=$(sed -n 's/^AWS_SECRET_ACCESS_KEY=//p' test-host.env)

# The host policy's statements, for user-policy.json (no Principal) and bucket-policy.json (Principal test-host), and
# with --maint-policy, holdfast-maint's statement in bucket-policy.json.
python3 -I - "$B" "$P" "$MECH" "$MAINT" <<'PY'
import json, sys
b, p, mech, maint = sys.argv[1:5]
host = [
    {"Sid": "ListOwnPrefix", "Effect": "Allow", "Action": "s3:ListBucket", "Resource": f"arn:aws:s3:::{b}",
     "Condition": {"StringLike": {"s3:prefix": [p, p + "/*"]}}},
    {"Sid": "ReadWriteOwnPrefix", "Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject"],
     "Resource": f"arn:aws:s3:::{b}/{p}/*"},
    {"Sid": "DeleteOwnLocks", "Effect": "Allow", "Action": "s3:DeleteObject", "Resource": f"arn:aws:s3:::{b}/{p}/locks/*"},
]
with open("user-policy.json", "w") as f:
    json.dump({"Version": "2012-10-17", "Statement": host}, f, indent=2)
bucket = [dict(s, Principal={"AWS": ["arn:aws:iam:::user/test-host"]}) for s in host] if mech == "bucket-policy" else []
if maint == "--maint-policy":
    bucket.append({"Sid": "MaintenanceAll", "Effect": "Allow", "Principal": {"AWS": ["arn:aws:iam:::user/holdfast-maint"]},
                   "Action": "s3:*", "Resource": [f"arn:aws:s3:::{b}", f"arn:aws:s3:::{b}/*"]})
with open("bucket-policy.json", "w") as f:
    json.dump({"Version": "2012-10-17", "Statement": bucket}, f, indent=2)
print(f"# bucket-policy.json has {len(bucket)} statements; user-policy.json {len(host)}")
PY

echo
echo "## 0. A clean start: no test-host, no bucket policy, nothing left under $P/ from a run before"
check - holdfast-provisioner s3req DELETE "$RGW/admin/user?uid=test-host&purge-data=true"
check - holdfast-maint s3req DELETE "$RGW/$B/?policy"
echo "\$ (holdfast-maint) every version and delete marker under $P/ removed"
s3py holdfast-maint 'n = 0
while True:
    r = c.list_object_versions(Bucket=sys.argv[2], Prefix=sys.argv[3])
    objs = [{"Key": v["Key"], "VersionId": v["VersionId"]} for v in r.get("Versions", []) + r.get("DeleteMarkers", [])]
    if not objs:
        break
    c.delete_objects(Bucket=sys.argv[2], Delete={"Objects": objs, "Quiet": True})
    n += len(objs)
print(f"    removed {n}")' "$P/"
check 2xx holdfast-maint s3req --data /etc/hostname PUT "$RGW/$B/$O/secret.txt"

echo
echo "## 1. test-host made through the Admin Ops API, as holdfast-provisioner"
check 200 holdfast-provisioner s3req PUT "$RGW/admin/user?uid=test-host&display-name=test-host&key-type=s3&access-key=$TH_KEY&secret-key=$TH_SECRET&format=json"
check 200 holdfast-provisioner s3req --body GET "$RGW/admin/user?uid=test-host&format=json"
echo "# the same create again (is it idempotent?)"
check - holdfast-provisioner s3req PUT "$RGW/admin/user?uid=test-host&display-name=test-host&key-type=s3&access-key=$TH_KEY&secret-key=$TH_SECRET&format=json"
echo "# the same key again, through the key call (PUT /admin/user?key)"
check - holdfast-provisioner s3req PUT "$RGW/admin/user?key&uid=test-host&key-type=s3&access-key=$TH_KEY&secret-key=$TH_SECRET&format=json"
echo "# for comparison, radosgw-admin's view of the user (secret keys left out)"
echo "\$ microceph.radosgw-admin user info --uid test-host"
/snap/bin/microceph.radosgw-admin user info --uid test-host | python3 -I -c 'import json, sys
d = json.load(sys.stdin)
print("    " + json.dumps({"user_id": d["user_id"], "access_keys": [k["access_key"] for k in d["keys"]],
                         "caps": d["caps"], "type": d.get("type"), "account_id": d.get("account_id", "")}))'
echo "# test-host's key is $TH_KEY (credentials/test-host.env in the lab's state)"

echo
echo "## 2. The host policy, as a $MECH$([[ $MAINT == none ]] || echo ", and holdfast-maint's statement in the bucket policy")"
if [[ $MECH == user-policy ]]; then
	check 200 holdfast-provisioner awsd iam put-user-policy --user-name test-host --policy-name holdfast-host --policy-document file:///root/hf-lab/user-policy.json
	check 200 holdfast-provisioner awsd iam list-user-policies --user-name test-host
	check 200 holdfast-provisioner awsd iam get-user-policy --user-name test-host --policy-name holdfast-host
else
	check 200 holdfast-provisioner awsd iam list-user-policies --user-name test-host
fi
if [[ $MECH == bucket-policy || $MAINT != none ]]; then
	check 2xx holdfast-maint awsd s3api put-bucket-policy --bucket "$B" --policy file:///root/hf-lab/bucket-policy.json
	check 200 holdfast-maint s3req --body GET "$RGW/$B/?policy"
else
	check 404 holdfast-maint s3req GET "$RGW/$B/?policy"
fi
check 200 holdfast-maint awsd s3api get-bucket-versioning --bucket "$B"
echo "# S3 Object Ownership (BucketOwnerEnforced: the bucket's owner owns every object), on a scratch bucket"
check - holdfast-maint awsd s3api create-bucket --bucket hf-probe-ownership
check - holdfast-maint awsd s3api put-bucket-ownership-controls --bucket hf-probe-ownership --ownership-controls 'Rules=[{ObjectOwnership=BucketOwnerEnforced}]'
check - holdfast-maint awsd s3api delete-bucket --bucket hf-probe-ownership

echo
echo "## 3. What test-host may do, and what it may not"
echo "# may: write, read and list under its own prefix"
check 2xx test-host s3req --data /etc/hostname --show-header x-amz-version-id PUT "$RGW/$B/$P/probe.txt"
check 2xx test-host s3req GET "$RGW/$B/$P/probe.txt"
check 2xx test-host s3req GET "$RGW/$B/?list-type=2&prefix=$P/"
check 2xx test-host s3req GET "$RGW/$B/?list-type=2&prefix=$P"
check 2xx test-host s3req GET "$RGW/$B/?list-type=2&prefix=$P/locks/"
echo "# may not: read, list or write another host's prefix, or list the bucket"
check 403 test-host s3req GET "$RGW/$B/$O/secret.txt"
check 403 test-host s3req GET "$RGW/$B/?list-type=2&prefix=$O/"
check 403 test-host s3req GET "$RGW/$B/?list-type=2"
check 403 test-host s3req GET "$RGW/$B/?list-type=2&prefix=repos/"
check 403 test-host s3req GET "$RGW/$B/?list-type=2&prefix=repos/test.example.co"
check 403 test-host s3req --data /etc/hostname PUT "$RGW/$B/$O/planted.txt"
check 403 test-host s3req GET "$RGW/$B/?versioning"
check 403 test-host s3req GET "$RGW/$B/?policy"
echo "# may not: delete in its own prefix but under locks/, or delete a version"
check 403 test-host s3req DELETE "$RGW/$B/$P/probe.txt"
V=$(s3py holdfast-maint 'print([v["VersionId"] for v in c.list_object_versions(Bucket=sys.argv[2], Prefix=sys.argv[3])["Versions"] if v["IsLatest"]][0])' "$P/probe.txt")
check 403 test-host s3req DELETE "$RGW/$B/$P/probe.txt?versionId=$V"
echo "# may: overwrite its own object (versioning keeps the version before)"
check 2xx test-host s3req --data /etc/os-release --show-header x-amz-version-id PUT "$RGW/$B/$P/probe.txt"
echo "# may: delete under its own locks/"
check 2xx test-host s3req --data /etc/hostname PUT "$RGW/$B/$P/locks/lock-1"
check 204 test-host s3req DELETE "$RGW/$B/$P/locks/lock-1"
echo "# may not: delete a lock's version for good"
V=$(s3py holdfast-maint 'print([v["VersionId"] for v in c.list_object_versions(Bucket=sys.argv[2], Prefix=sys.argv[3])["Versions"]][0])' "$P/locks/lock-1")
check 403 test-host s3req DELETE "$RGW/$B/$P/locks/lock-1?versionId=$V"
echo "# what holdfast-maint sees under $P/: every version is still there, and who owns it"
echo "\$ (holdfast-maint) list-object-versions $P/"
s3py holdfast-maint 'r = c.list_object_versions(Bucket=sys.argv[2], Prefix=sys.argv[3])
for v in r.get("Versions", []):
    print("    version  %s  %s  latest=%s  owner=%s" % (v["Key"], v["VersionId"], v["IsLatest"], v.get("Owner", {}).get("ID", "?")))
for d in r.get("DeleteMarkers", []):
    print("    deleted  %s  %s  latest=%s" % (d["Key"], d["VersionId"], d["IsLatest"]))' "$P/"
echo "# holdfast-maint, the bucket's owner and the maintenance identity: does it read, and delete, what test-host wrote?"
check 2xx holdfast-maint s3req GET "$RGW/$B/$P/probe.txt"
check 2xx holdfast-maint s3req --data /etc/hostname PUT "$RGW/$B/$P/maint-probe.txt"
check 204 holdfast-maint s3req DELETE "$RGW/$B/$P/probe.txt"
EOF

# Step 4, restic on an agent.
echo
echo "## 4. restic from $agent: init as holdfast-maint, backup and forget as test-host, check and prune as holdfast-maint"
vm_ssh "$agent" bash -s -- "$HF_REPO_BASE/$fqdn" "$HF_REGION" <<'EOF' | tee -a "$out" || true
set -u
export RESTIC_REPOSITORY=$1 AWS_DEFAULT_REGION=$2 RESTIC_PASSWORD_FILE=/root/hf-lab/test-host.restic.pw
R=/usr/libexec/holdfast/restic
as() { local u=$1; shift; ( set -a; . "/root/hf-lab/$u.env"; set +a; "$@" ); }
# run <ok|fails> <user> <command>...: PASS when restic exits 0 (ok) or not (fails), FAIL otherwise.
run() {
	local want=$1 who=$2 res rc
	shift 2
	echo "\$ ($who) RESTIC_REPOSITORY=$RESTIC_REPOSITORY $*"
	res=$(as "$who" "$@" 2>&1); rc=$?
	grep -vE '^\s+(/|main\.|runtime\.|github\.com)' <<<"$res" | tail -n 12 | sed 's/^/    /'
	if [[ ($want == ok && $rc == 0) || ($want == fails && $rc != 0) ]]; then
		echo "PASS  $who: exit $rc, as wanted ($want)"
	else
		echo "FAIL  $who: exit $rc, wanted $want"
	fi
}
run ok holdfast-maint "$R" --no-cache init
run ok test-host "$R" --no-cache backup /etc/hostname /etc/os-release /etc/hosts --host test.example.com
run ok test-host "$R" --no-cache snapshots --compact
id=$(as test-host "$R" --no-cache snapshots --json | python3 -I -c 'import json, sys; print(json.load(sys.stdin)[-1]["short_id"])')
run fails test-host "$R" --no-cache forget "$id"
run ok test-host "$R" --no-cache snapshots --compact
run ok holdfast-maint "$R" --no-cache check --read-data
run ok test-host "$R" --no-cache backup /etc/hostname /etc/os-release /etc/hosts /etc/fstab --host test.example.com
run ok holdfast-maint "$R" --no-cache forget --keep-last 1 --prune
run ok holdfast-maint "$R" --no-cache snapshots --compact
EOF

pass=$(grep -c '^PASS ' "$out" || true) fail=$(grep -c '^FAIL ' "$out" || true) info=$(grep -c '^INFO ' "$out" || true)
echo
echo "# results ($mech${maint:+ $maint}): $pass PASS, $fail FAIL, $info INFO"
grep '^FAIL ' "$out" | sed 's/^/#   /' || true
((fail == 0))
