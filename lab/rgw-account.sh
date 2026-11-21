#!/usr/bin/env bash
# lab/rgw-account.sh <lab>: the RGW-accounts test on a ready lab (lab/README.md, The RGW experiment). It answers one
# question: does an RGW account (Squid) let one maintenance key read and delete every host's objects while each host
# key stays limited to its own prefix? On the storage VM, as root with radosgw-admin, it makes a scratch account
# hf-acct-test, its users acct-provisioner (managed policy IAMFullAccess), acct-maint and acct-host, and the bucket
# hf-acct-test, versioned; as acct-provisioner, through the IAM API, it puts acct-maint's policy (s3:* on the bucket)
# and acct-host's (Holdfast's host policy for repos/test.example.com/). Then the data plane as acct-host and
# acct-maint, and restic from an agent: init (maint), backup (host), check --read-data and forget --prune (maint).
# Last, it removes the scratch account, its users and its bucket again. Keys: credentials/acct-*.env in the lab's
# state. Exits 0 when every step went as wanted, 1 when not, 3 when the lab is not up.
set -euo pipefail
cd "$(dirname "$0")/.."
lab=${1:-}
[[ $lab =~ ^[a-z0-9]+$ ]] || { sed -n '2,/^set -e/{/^set -e/d; s/^# \{0,1\}//; p}' "$0" >&2; exit 2; }
state=${XDG_STATE_HOME:-$HOME/.local/state}/holdfast/labs/$lab
[[ -f $state/lab.env ]] || { echo "rgw-account: the lab $lab is not up (lab/lab up $lab)" >&2; exit 3; }
# shellcheck source=/dev/null
source "$state/lab.env"
creds=$HF_STATE/credentials agent=${HF_AGENTS[0]}
for u in provisioner maint host; do python3 lab/ansible/files/credential.py s3 "$creds/acct-$u.env" "ACCT${u:0:4}"; done
python3 lab/ansible/files/credential.py password "$creds/test-host.restic.pw"
vm_ssh() {
	local vm=$1 ip
	shift
	ip=HF_VM_IP_$vm
	ssh -i "$HF_SSH_KEY" -o IdentitiesOnly=yes -o "UserKnownHostsFile=$HF_STATE/known_hosts" -o StrictHostKeyChecking=accept-new \
		-o LogLevel=ERROR -o BatchMode=yes "root@${!ip}" "$@"
}
put() { vm_ssh "$1" "umask 077 && mkdir -p /root/hf-lab && cat >/root/hf-lab/$3" <"$2"; }
for u in provisioner maint host; do put "$HF_STORAGE_VM" "$creds/acct-$u.env" "acct-$u.env"; done
for u in maint host; do put "$agent" "$creds/acct-$u.env" "acct-$u.env"; done
put "$agent" "$creds/test-host.restic.pw" test-host.restic.pw
out=$(mktemp) && trap 'rm -f "$out"' EXIT
echo "# RGW accounts test: the lab $lab, $(date -u +%FT%TZ)"
sed 's/^/# /' "$HF_CEPH_VERSION_FILE"

# setup <lab's RGW URL>: the scratch account, users, policies and bucket; then the data plane.
vm_ssh "$HF_STORAGE_VM" bash -s -- "$HF_RGW_URL" <<'EOF' | tee "$out" || true
set -u
E=$1 ACC=RGW79200000000000001 B=hf-acct-test P=repos/test.example.com O=repos/other.example.com
A=/snap/bin/microceph.radosgw-admin
cd /root/hf-lab
export PATH=$PATH:/snap/bin AWS_PAGER= AWS_DEFAULT_REGION=default
as() { local u=$1; shift; ( set -a; . "/root/hf-lab/acct-$u.env"; set +a; "$@" ); }
key() { sed -n "s/^AWS_$2=//p" "/root/hf-lab/acct-$1.env"; }
j() { python3 -I -c 'import json,sys; d=json.load(sys.stdin); print("    " + json.dumps({k: d.get(k) for k in sys.argv[1:]}))' "$@"; }
check() {
	local want=$1 who=$2 res st code
	shift 2
	echo "\$ (acct-$who) $*"
	res=$(as "$who" "$@" 2>&1)
	sed 's/^/    /' <<<"$res"
	read -r st code < <(sed -n 's/^[A-Z]* .* -> \([0-9][0-9][0-9]\) *\([A-Za-z]*\).*/\1 \2/p' <<<"$res" | head -1)
	if [[ ($want == 403 && $st == 403 && $code == AccessDenied) || ($want != 403 && $st == "$want") || ($want == 2xx && $st == 2??) ]]; then
		echo "PASS  acct-$who: $st${code:+ $code}, as wanted ($want)"
	else
		echo "FAIL  acct-$who: ${st:-no status}${code:+ $code}, wanted $want"
	fi
}
iam() { # iam <user> <aws iam arguments>: PASS when aws exits 0
	local who=$1 rc
	shift
	echo "\$ (acct-$who) aws iam $*"
	as "$who" aws --endpoint-url "$E" iam "$@" 2>&1 | sed 's/^/    /'; rc=${PIPESTATUS[0]}
	if ((rc == 0)); then echo "PASS  acct-$who: aws exit 0"; else echo "FAIL  acct-$who: aws exit $rc"; fi
}
echo
echo "## setup, as root on the storage VM (radosgw-admin): the account, its users, the provisioner's managed policy"
echo "\$ radosgw-admin account create --account-name hf-acct-test --account-id $ACC"
$A account create --account-name hf-acct-test --account-id $ACC | j id name
for u in provisioner maint host; do
	echo "\$ radosgw-admin user create --uid acct-$u --display-name acct-$u --account-id $ACC --access-key $(key $u ACCESS_KEY_ID) --secret-key ***"
	$A user create --uid acct-$u --display-name acct-$u --account-id $ACC --access-key "$(key $u ACCESS_KEY_ID)" --secret-key "$(key $u SECRET_ACCESS_KEY)" | j user_id account_id
done
echo "\$ radosgw-admin user policy attach --uid acct-provisioner --policy-arn arn:aws:iam::aws:policy/IAMFullAccess"
$A user policy attach --uid acct-provisioner --policy-arn arn:aws:iam::aws:policy/IAMFullAccess | sed 's/^/    /'
cat >acct-maint-policy.json <<J
{"Version": "2012-10-17", "Statement": [{"Sid": "MaintenanceAll", "Effect": "Allow", "Action": "s3:*",
  "Resource": ["arn:aws:s3:::$B", "arn:aws:s3:::$B/*"]}]}
J
cat >acct-host-policy.json <<J
{"Version": "2012-10-17", "Statement": [
  {"Sid": "ListOwnPrefix", "Effect": "Allow", "Action": "s3:ListBucket", "Resource": "arn:aws:s3:::$B",
   "Condition": {"StringLike": {"s3:prefix": ["$P", "$P/*"]}}},
  {"Sid": "ReadWriteOwnPrefix", "Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject"], "Resource": "arn:aws:s3:::$B/$P/*"},
  {"Sid": "DeleteOwnLocks", "Effect": "Allow", "Action": "s3:DeleteObject", "Resource": "arn:aws:s3:::$B/$P/locks/*"}]}
J
echo
echo "## the policies, as acct-provisioner through the IAM API"
iam provisioner put-user-policy --user-name acct-maint --policy-name holdfast-maintenance --policy-document file:///root/hf-lab/acct-maint-policy.json
iam provisioner put-user-policy --user-name acct-host --policy-name holdfast-host --policy-document file:///root/hf-lab/acct-host-policy.json
echo
echo "## the bucket, made by acct-maint: the account owns it"
check 200 maint s3req PUT "$E/$B"
check 200 maint s3req --data /dev/stdin PUT "$E/$B/?versioning" <<<'<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>'
check 200 maint s3req --body GET "$E/$B/?versioning"
echo "\$ radosgw-admin bucket stats --bucket $B"
$A bucket stats --bucket $B | j bucket owner
check 2xx maint s3req --data /etc/hostname PUT "$E/$B/$O/secret.txt"
echo
echo "## acct-host: its own prefix only, deletes only under locks/"
check 2xx host s3req --data /etc/hostname PUT "$E/$B/$P/probe.txt"
check 2xx host s3req GET "$E/$B/$P/probe.txt"
check 2xx host s3req GET "$E/$B/?list-type=2&prefix=$P/"
check 403 host s3req GET "$E/$B/$O/secret.txt"
check 403 host s3req GET "$E/$B/?list-type=2&prefix=$O/"
check 403 host s3req GET "$E/$B/?list-type=2"
check 403 host s3req --data /etc/hostname PUT "$E/$B/$O/planted.txt"
check 403 host s3req DELETE "$E/$B/$P/probe.txt"
check 2xx host s3req --data /etc/hostname PUT "$E/$B/$P/locks/lock-1"
check 204 host s3req DELETE "$E/$B/$P/locks/lock-1"
echo
echo "## acct-maint: reads and deletes what acct-host wrote, under any prefix"
check 200 maint s3req GET "$E/$B/$P/probe.txt"
check 200 maint s3req GET "$E/$B/?list-type=2&prefix=repos/"
check 204 maint s3req DELETE "$E/$B/$P/probe.txt"
check 204 maint s3req DELETE "$E/$B/$O/secret.txt"
EOF

echo
echo "## restic from $agent: init as acct-maint, backup as acct-host, check --read-data and forget --prune as acct-maint"
vm_ssh "$agent" bash -s -- "s3:$HF_RGW_URL/hf-acct-test/repos/test.example.com" <<'EOF' | tee -a "$out" || true
set -u
export RESTIC_REPOSITORY=$1 AWS_DEFAULT_REGION=default RESTIC_PASSWORD_FILE=/root/hf-lab/test-host.restic.pw
R=/usr/libexec/holdfast/restic
as() { local u=$1; shift; ( set -a; . "/root/hf-lab/acct-$u.env"; set +a; "$@" ); }
run() {
	local want=$1 who=$2 res rc
	shift 2
	echo "\$ (acct-$who) RESTIC_REPOSITORY=$RESTIC_REPOSITORY $*"
	res=$(as "$who" "$@" 2>&1); rc=$?
	grep -vE '^\s+(/|main\.|runtime\.|github\.com)' <<<"$res" | tail -n 10 | sed 's/^/    /'
	if [[ ($want == ok && $rc == 0) || ($want == fails && $rc != 0) ]]; then echo "PASS  acct-$who: exit $rc, as wanted ($want)"; else echo "FAIL  acct-$who: exit $rc, wanted $want"; fi
}
run ok maint "$R" --no-cache init
run ok host "$R" --no-cache backup /etc/hostname /etc/os-release --host test.example.com
id=$(as host "$R" --no-cache snapshots --json | python3 -I -c 'import json, sys; print(json.load(sys.stdin)[-1]["short_id"])')
run fails host "$R" --no-cache forget "$id"
run ok maint "$R" --no-cache check --read-data
run ok host "$R" --no-cache backup /etc/hostname /etc/os-release /etc/hosts --host test.example.com
run ok maint "$R" --no-cache forget --keep-last 1 --group-by host --prune
run ok maint "$R" --no-cache snapshots --compact
EOF

echo
echo "## cleanup: the scratch account, its users and bucket removed (radosgw-admin)"
vm_ssh "$HF_STORAGE_VM" bash -s <<'EOF'
A=/snap/bin/microceph.radosgw-admin
for u in acct-host acct-maint acct-provisioner; do $A user rm --uid $u && echo "    user rm $u"; done
$A bucket rm --bucket hf-acct-test --purge-objects && echo "    bucket rm hf-acct-test"
$A account rm --account-id RGW79200000000000001 && echo "    account rm RGW79200000000000001"
EOF
pass=$(grep -c '^PASS ' "$out" || true) fail=$(grep -c '^FAIL ' "$out" || true)
echo "# results (accounts): $pass PASS, $fail FAIL"
grep '^FAIL ' "$out" | sed 's/^/#   /' || true
((fail == 0))
