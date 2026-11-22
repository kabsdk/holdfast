#!/usr/bin/env bash
# lab/rgw-documented-layout.sh [--keep-data]: the lab bucket `holdfast` on s3a in the layout docs/storage.md
# describes, so Holdfast's contract test (B001-B012) can pass against real Ceph RGW. Run it on s3a as root:
#
#   lab/lab ssh m1 s3a 'bash -s' < lab/rgw-documented-layout.sh                  rebuilds the bucket: DELETES its data
#   lab/lab ssh m1 s3a 'bash -s -- --keep-data' < lab/rgw-documented-layout.sh   keeps it: the users and the policy only
#
# Without --keep-data it DELETES the bucket and everything in it (lab data only), then makes it again. With
# --keep-data it keeps a bucket that is already in this layout (owned by holdfast-bucket-owner) and its data, makes it
# when it is missing or when another identity owns it but it holds no object (as lab/lab up makes it), and refuses one
# that another identity owns and that holds data. lab/lab test m1 runs it with --keep-data. Either way:
#   - owned by `holdfast-bucket-owner`, an identity of the admin's own, not by the maintenance key (B010);
#   - versioning on (B008), and a lifecycle rule that expires noncurrent versions in the whole bucket (B011);
#   - a bucket policy: the maintenance user `holdfast-maint` may read, write, list and delete under repos/,
#     including what host users wrote (B003, B004), write and delete under escrow/ (B012), and is denied version
#     deletes (B009); each host user gets
#     docs/storage.md's host statements for its own prefix, repos/<fqdn>/, and may delete only under its locks/
#     (B001-B007).
# The host users: `test-host` for repos/test.example.com/ (the isolation experiments' and TestS3ProberLive's), and one
# per line "<user> <fqdn>" of /root/hf-lab/hosts, which lab/lab test writes (host-<vm> for <vm>.m1.holdfast.test).
# Each user's key is /root/hf-lab/<user>.env (0600, put there from the lab's state, credentials/<user>.env); a user
# that is missing is made with that key, and one that lacks it is given it. No key is printed.
# Plain RGW users, no account: the bucket-policy grant is what lets the maintenance user read the host's
# objects (docs/storage.md, "If B003 fails").
set -euo pipefail
keep=0
case $# in
0) ;;
1) [[ $1 == --keep-data ]] || { echo "rgw-documented-layout.sh takes --keep-data, not '$1'" >&2; exit 2; }; keep=1 ;;
*) echo "rgw-documented-layout.sh takes --keep-data, not '$*'" >&2; exit 2 ;;
esac
cd /root/hf-lab
RGW=http://s3a.m1.holdfast.test
B=holdfast
export PATH=$PATH:/snap/bin AWS_PAGER=
# This script is read from stdin (bash -s): nothing in it may read stdin, so every command gets </dev/null.
as() { local u=$1; shift; (set -a; . "/root/hf-lab/$u.env"; set +a; "$@" </dev/null); }
key_id() { sed -n 's/^AWS_ACCESS_KEY_ID=//p' "/root/hf-lab/$1.env"; }

if ! radosgw-admin user info --uid holdfast-bucket-owner >/dev/null 2>&1 </dev/null; then
	radosgw-admin user create --uid holdfast-bucket-owner --display-name "Holdfast bucket owner (the admin's identity)" >/dev/null </dev/null
fi
(umask 077; radosgw-admin user info --uid holdfast-bucket-owner </dev/null | python3 -c 'import json, sys
k = json.load(sys.stdin)["keys"][0]
print("AWS_ACCESS_KEY_ID=" + k["access_key"]); print("AWS_SECRET_ACCESS_KEY=" + k["secret_key"])' >holdfast-bucket-owner.env)

# The host users, each with the key in its file.
hosts=("test-host test.example.com")
if [[ -f hosts ]]; then
	while read -r u f _; do
		[[ -z $u || $u == \#* ]] || hosts+=("$u $f")
	done <hosts
fi
for h in "${hosts[@]}"; do
	read -r u f <<<"$h"
	[[ $u =~ ^[a-z0-9][a-z0-9-]*$ && $f =~ ^[a-z0-9][a-z0-9.-]*$ ]] || { echo "refused: '$u $f' is not '<user> <fqdn>' (/root/hf-lab/hosts)" >&2; exit 1; }
	[[ -s $u.env ]] || { echo "refused: no key for $u: /root/hf-lab/$u.env is missing" >&2; exit 1; }
	k=$(key_id "$u")
	if ! info=$(radosgw-admin user info --uid "$u" 2>/dev/null </dev/null); then
		(set -a; . "./$u.env"; set +a
			radosgw-admin user create --uid "$u" --display-name "Holdfast host $f" --access-key "$AWS_ACCESS_KEY_ID" \
				--secret-key "$AWS_SECRET_ACCESS_KEY" >/dev/null </dev/null)
		echo "user $u: made, with the key in /root/hf-lab/$u.env (for repos/$f/)"
	elif ! python3 -c 'import json, sys; sys.exit(sys.argv[1] not in [x["access_key"] for x in json.load(sys.stdin)["keys"]])' "$k" <<<"$info"; then
		(set -a; . "./$u.env"; set +a
			radosgw-admin key create --uid "$u" --key-type s3 --access-key "$AWS_ACCESS_KEY_ID" \
				--secret-key "$AWS_SECRET_ACCESS_KEY" >/dev/null </dev/null)
		echo "user $u: given the key in /root/hf-lab/$u.env"
	fi
done
unset info

# The bucket's owner and how many objects (every version) it holds; nothing when it is missing.
read -r owner objects < <(radosgw-admin bucket stats --bucket "$B" 2>/dev/null </dev/null | python3 -c 'import json, sys
try:
    d = json.load(sys.stdin)
    print(d["owner"], sum(u.get("num_objects", 0) for u in (d.get("usage") or {}).values()))
except Exception:
    print("")' || true)
rebuild=1
if ((keep)); then
	if [[ -z ${owner:-} ]]; then
		echo "bucket $B is missing: making it"
		rebuild=0
	elif [[ $owner == holdfast-bucket-owner ]]; then
		echo "keeping bucket $B and its data"
		rebuild=-1
	elif [[ ${objects:-1} == 0 ]]; then
		echo "bucket $B is owned by $owner but empty (as lab/lab up makes it): making it again, owned by holdfast-bucket-owner"
	else
		echo "refused: bucket $B is owned by $owner and holds $objects objects, so it is not in this layout (lab/lab isolation replaces it). Run this script without --keep-data, which deletes the bucket's data and makes it again" >&2
		exit 1
	fi
else
	echo "removing bucket $B and its data (lab only)"
fi
if ((rebuild == 1)); then
	radosgw-admin bucket rm --bucket="$B" --purge-objects --bypass-gc >/dev/null 2>&1 </dev/null || true
fi
if ((rebuild >= 0)); then
	as holdfast-bucket-owner aws --endpoint-url "$RGW" s3api create-bucket --bucket "$B" >/dev/null
fi
as holdfast-bucket-owner aws --endpoint-url "$RGW" s3api put-bucket-versioning --bucket "$B" --versioning-configuration Status=Enabled

# The whole bucket (docs/storage.md): repos/, and the escrow files' old versions under escrow/.
cat >lifecycle.json <<'EOF'
{"Rules": [{"ID": "expire-noncurrent", "Status": "Enabled", "Filter": {"Prefix": ""},
  "NoncurrentVersionExpiration": {"NoncurrentDays": 30}}]}
EOF
as holdfast-bucket-owner aws --endpoint-url "$RGW" s3api put-bucket-lifecycle-configuration --bucket "$B" --lifecycle-configuration file://lifecycle.json

# The maintenance statements, then docs/storage.md's four host statements for each host user, on its own prefix.
python3 -I -c '
import json, sys
b, hosts = sys.argv[1], [h.split() for h in sys.argv[2:]]
m = {"AWS": ["arn:aws:iam:::user/holdfast-maint"]}
st = [
    {"Sid": "MaintBucket", "Effect": "Allow", "Principal": m,
     "Action": ["s3:ListBucket", "s3:ListBucketVersions", "s3:GetBucketLocation", "s3:GetBucketVersioning",
                "s3:GetLifecycleConfiguration"],
     "Resource": [f"arn:aws:s3:::{b}"]},
    {"Sid": "MaintObjects", "Effect": "Allow", "Principal": m,
     "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"], "Resource": [f"arn:aws:s3:::{b}/repos/*"]},
    {"Sid": "MaintEscrow", "Effect": "Allow", "Principal": m,
     "Action": ["s3:PutObject", "s3:DeleteObject"], "Resource": [f"arn:aws:s3:::{b}/escrow/*"]},
    {"Sid": "MaintNoVersionDeletes", "Effect": "Deny", "Principal": m,
     "Action": ["s3:DeleteObjectVersion"], "Resource": [f"arn:aws:s3:::{b}/*"]},
]
for user, fqdn in hosts:
    h, p = {"AWS": [f"arn:aws:iam:::user/{user}"]}, f"repos/{fqdn}"
    sid = "".join(w.capitalize() for w in user.split("-"))
    st += [
        {"Sid": "HostLocation" + sid, "Effect": "Allow", "Principal": h, "Action": ["s3:GetBucketLocation"],
         "Resource": [f"arn:aws:s3:::{b}"]},
        {"Sid": "HostList" + sid, "Effect": "Allow", "Principal": h, "Action": ["s3:ListBucket"], "Resource": [f"arn:aws:s3:::{b}"],
         "Condition": {"StringLike": {"s3:prefix": [p, p + "/*"]}}},
        {"Sid": "HostObjects" + sid, "Effect": "Allow", "Principal": h, "Action": ["s3:GetObject", "s3:PutObject"],
         "Resource": [f"arn:aws:s3:::{b}/{p}/*"]},
        {"Sid": "HostLocks" + sid, "Effect": "Allow", "Principal": h, "Action": ["s3:DeleteObject"],
         "Resource": [f"arn:aws:s3:::{b}/{p}/locks/*"]},
    ]
json.dump({"Version": "2012-10-17", "Statement": st}, sys.stdout, indent=1)
print()
' "$B" "${hosts[@]}" >documented-policy.json </dev/null
as holdfast-bucket-owner aws --endpoint-url "$RGW" s3api put-bucket-policy --bucket "$B" --policy file://documented-policy.json
echo "bucket $B: owner $(radosgw-admin bucket stats --bucket "$B" </dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin)["owner"])'), versioning $(as holdfast-bucket-owner aws --endpoint-url "$RGW" s3api get-bucket-versioning --bucket "$B" --query Status --output text)"
echo "bucket policy: holdfast-maint, and the host users $(for h in "${hosts[@]}"; do read -r u f <<<"$h"; printf '%s (repos/%s/) ' "$u" "$f"; done)"
