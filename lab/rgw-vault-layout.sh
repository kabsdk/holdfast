#!/usr/bin/env bash
# lab/rgw-vault-layout.sh: site B's bucket `holdfast-offsite` on s3a, for the vault (M7, docs/storage.md "The vault's
# keys"), in the documented layout. Run it on s3a as root: lab/lab ssh m1 s3a 'bash -s' < lab/rgw-vault-layout.sh.
# lab/test-vault.sh runs it. It keeps the bucket and its data when they exist, and makes them otherwise:
#   - owned by `holdfast-bucket-owner` (rgw-documented-layout.sh makes it), with versioning and a lifecycle rule;
#   - a bucket policy: the vault's maintenance user `holdfast-vault-maint` may do what a maintenance key may, with the
#     deny on version deletes; each line "<user> <fqdn>" of /root/hf-lab/offsite-hosts is a restore user, which may
#     read and list repos/<fqdn>/ only.
# Each user's key is /root/hf-lab/<user>.env (0600), put there from the lab's state; a user that is missing is made
# with it. No key is printed. The lab has one RGW: site B is another bucket on it, not another realm or cluster, which
# docs/storage.md recommends and Holdfast cannot test.
set -euo pipefail
cd /root/hf-lab
RGW=http://s3a.m1.holdfast.test
B=holdfast-offsite
export PATH=$PATH:/snap/bin AWS_PAGER=
as() { local u=$1; shift; (set -a; . "/root/hf-lab/$u.env"; set +a; "$@" </dev/null); }
key_id() { sed -n 's/^AWS_ACCESS_KEY_ID=//p' "/root/hf-lab/$1.env"; }
[[ -s holdfast-bucket-owner.env ]] || { echo "refused: no holdfast-bucket-owner: run lab/rgw-documented-layout.sh first" >&2; exit 1; }

users=("holdfast-vault-maint vault")
if [[ -f offsite-hosts ]]; then
	while read -r u f _; do
		[[ -z $u || $u == \#* ]] || users+=("$u $f")
	done <offsite-hosts
fi
for h in "${users[@]}"; do
	read -r u f <<<"$h"
	[[ $u =~ ^[a-z0-9][a-z0-9-]*$ && $f =~ ^[a-z0-9][a-z0-9.-]*$ ]] || { echo "refused: '$u $f' is not '<user> <fqdn>' (/root/hf-lab/offsite-hosts)" >&2; exit 1; }
	[[ -s $u.env ]] || { echo "refused: no key for $u: /root/hf-lab/$u.env is missing" >&2; exit 1; }
	if ! info=$(radosgw-admin user info --uid "$u" 2>/dev/null </dev/null); then
		(set -a; . "./$u.env"; set +a
			radosgw-admin user create --uid "$u" --display-name "Holdfast vault $u" --access-key "$AWS_ACCESS_KEY_ID" \
				--secret-key "$AWS_SECRET_ACCESS_KEY" >/dev/null </dev/null)
		echo "user $u: made, with the key in /root/hf-lab/$u.env"
	elif ! python3 -c 'import json, sys; sys.exit(sys.argv[1] not in [x["access_key"] for x in json.load(sys.stdin)["keys"]])' "$(key_id "$u")" <<<"$info"; then
		(set -a; . "./$u.env"; set +a
			radosgw-admin key create --uid "$u" --key-type s3 --access-key "$AWS_ACCESS_KEY_ID" --secret-key "$AWS_SECRET_ACCESS_KEY" >/dev/null </dev/null)
		echo "user $u: given the key in /root/hf-lab/$u.env"
	fi
done
unset info

owner=$(radosgw-admin bucket stats --bucket "$B" 2>/dev/null </dev/null | python3 -c 'import json, sys
try:
    print(json.load(sys.stdin)["owner"])
except Exception:
    print("")' || true)
case $owner in
"") as holdfast-bucket-owner aws --endpoint-url "$RGW" s3api create-bucket --bucket "$B" >/dev/null; echo "bucket $B: made" ;;
holdfast-bucket-owner) echo "keeping bucket $B and its data" ;;
*) echo "refused: bucket $B is owned by $owner, not holdfast-bucket-owner" >&2; exit 1 ;;
esac
as holdfast-bucket-owner aws --endpoint-url "$RGW" s3api put-bucket-versioning --bucket "$B" --versioning-configuration Status=Enabled
cat >lifecycle-offsite.json <<'EOF'
{"Rules": [{"ID": "expire-noncurrent", "Status": "Enabled", "Filter": {"Prefix": ""},
  "NoncurrentVersionExpiration": {"NoncurrentDays": 30}}]}
EOF
as holdfast-bucket-owner aws --endpoint-url "$RGW" s3api put-bucket-lifecycle-configuration --bucket "$B" --lifecycle-configuration file://lifecycle-offsite.json

python3 -I -c '
import json, sys
b, rest = sys.argv[1], [h.split() for h in sys.argv[2:]]
m = {"AWS": ["arn:aws:iam:::user/holdfast-vault-maint"]}
st = [
    {"Sid": "MaintBucket", "Effect": "Allow", "Principal": m,
     "Action": ["s3:ListBucket", "s3:ListBucketVersions", "s3:GetBucketLocation", "s3:GetBucketVersioning", "s3:GetLifecycleConfiguration"],
     "Resource": [f"arn:aws:s3:::{b}"]},
    {"Sid": "MaintObjects", "Effect": "Allow", "Principal": m,
     "Action": ["s3:GetObject", "s3:GetObjectVersion", "s3:PutObject", "s3:DeleteObject"], "Resource": [f"arn:aws:s3:::{b}/repos/*"]},
    {"Sid": "MaintEscrow", "Effect": "Allow", "Principal": m, "Action": ["s3:PutObject", "s3:DeleteObject"], "Resource": [f"arn:aws:s3:::{b}/escrow/*"]},
    {"Sid": "MaintNoVersionDeletes", "Effect": "Deny", "Principal": m, "Action": ["s3:DeleteObjectVersion"], "Resource": [f"arn:aws:s3:::{b}/*"]},
]
for user, fqdn in rest:
    if user == "holdfast-vault-maint":
        continue
    r, p, sid = {"AWS": [f"arn:aws:iam:::user/{user}"]}, f"repos/{fqdn}", "".join(w.capitalize() for w in user.split("-"))
    st += [
        {"Sid": "RestoreLocation" + sid, "Effect": "Allow", "Principal": r, "Action": ["s3:GetBucketLocation"], "Resource": [f"arn:aws:s3:::{b}"]},
        {"Sid": "RestoreList" + sid, "Effect": "Allow", "Principal": r, "Action": ["s3:ListBucket"], "Resource": [f"arn:aws:s3:::{b}"],
         "Condition": {"StringLike": {"s3:prefix": [p, p + "/*"]}}},
        {"Sid": "RestoreObjects" + sid, "Effect": "Allow", "Principal": r, "Action": ["s3:GetObject"], "Resource": [f"arn:aws:s3:::{b}/{p}/*"]},
    ]
json.dump({"Version": "2012-10-17", "Statement": st}, sys.stdout, indent=1)
print()
' "$B" "${users[@]}" >offsite-policy.json </dev/null
as holdfast-bucket-owner aws --endpoint-url "$RGW" s3api put-bucket-policy --bucket "$B" --policy file://offsite-policy.json
echo "bucket $B: versioning $(as holdfast-bucket-owner aws --endpoint-url "$RGW" s3api get-bucket-versioning --bucket "$B" --query Status --output text); policy for holdfast-vault-maint and $((${#users[@]} - 1)) restore users"
