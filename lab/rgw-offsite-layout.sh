#!/usr/bin/env bash
# lab/rgw-offsite-layout.sh: the off-site bucket `holdfast-offsite` on s3a (spec §3.7, docs/storage.md "The off-site
# bucket"), in the documented layout. Run it on s3a as root: lab/lab ssh m1 s3a 'bash -s' < lab/rgw-offsite-layout.sh.
# lab/test-fidelity.sh runs it after lab/rgw-documented-layout.sh. It keeps the bucket and its data when they exist,
# and makes them otherwise:
#   - owned by `holdfast-bucket-owner` (rgw-documented-layout.sh makes it), with versioning and a lifecycle rule;
#   - a bucket policy: the off-site user `holdfast-offsite` may do what a maintenance key may, with the deny on version
#     deletes. lab/ansible/site.yml makes the user, and the server has its key (offsite.maintenance_credentials_file).
# No host has a key here: a restore from off-site reads the copy through the server's gateway (decision 041). The lab
# has one RGW: the off-site bucket is another bucket on it, not another realm or cluster, which docs/storage.md
# recommends and Holdfast cannot test.
set -euo pipefail
cd /root/hf-lab
RGW=http://s3a.m1.holdfast.test
B=holdfast-offsite
export PATH=$PATH:/snap/bin AWS_PAGER=
as() { local u=$1; shift; (set -a; . "/root/hf-lab/$u.env"; set +a; "$@" </dev/null); }
[[ -s holdfast-bucket-owner.env ]] || { echo "refused: no holdfast-bucket-owner: run lab/rgw-documented-layout.sh first" >&2; exit 1; }
radosgw-admin user info --uid holdfast-offsite >/dev/null 2>&1 </dev/null || { echo "refused: no user holdfast-offsite: lab/lab up makes it (lab/ansible/site.yml)" >&2; exit 1; }

# M7's vault, gone in M8 (decision 041): on a lab made before M8, its users go once, with their key files, and so does
# the bucket with the copies the vault made, whose passwords were in the vault's own database. The bucket is then made
# again, empty, below. The bucket goes first, so a run that stops halfway does it again.
m7=$(radosgw-admin user list </dev/null | python3 -c 'import json, re, sys
print(" ".join(u for u in json.load(sys.stdin) if re.fullmatch(r"hfb-[a-z0-9]+|holdfast-vault-(read|maint)", u)))')
if [[ -n $m7 ]]; then
	if radosgw-admin bucket stats --bucket "$B" >/dev/null 2>&1 </dev/null; then
		radosgw-admin bucket rm --bucket="$B" --purge-objects --bypass-gc >/dev/null 2>&1 </dev/null || true
		! radosgw-admin bucket stats --bucket "$B" >/dev/null 2>&1 </dev/null || { echo "failed: bucket $B, with M7's copies, could not be removed" >&2; exit 1; }
		echo "bucket $B: removed with M7's copies, which the server cannot read (their passwords were in the vault's database)"
	fi
	for u in $m7; do
		radosgw-admin user rm --uid "$u" >/dev/null </dev/null
		rm -f "$u.env"
		echo "user $u: removed (M7's vault)"
	done
	rm -f offsite-hosts
fi

owner=$(radosgw-admin bucket stats --bucket "$B" 2>/dev/null </dev/null | python3 -c 'import json, sys
try:
    print(json.load(sys.stdin)["owner"])
except Exception:
    print("")' || true)
# A bucket the off-site key owns is not in this layout (B010): restic init made it, when Holdfast still let it (before
# the copier tested the storage first). Its data are copies the server makes again, so it goes (lab data only).
if [[ $owner == holdfast-offsite ]]; then
	radosgw-admin bucket rm --bucket="$B" --purge-objects --bypass-gc >/dev/null 2>&1 </dev/null || true
	! radosgw-admin bucket stats --bucket "$B" >/dev/null 2>&1 </dev/null || { echo "failed: bucket $B, owned by holdfast-offsite, could not be removed" >&2; exit 1; }
	echo "bucket $B: removed; holdfast-offsite owned it, not holdfast-bucket-owner"
	owner=
fi
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
b = sys.argv[1]
m = {"AWS": ["arn:aws:iam:::user/holdfast-offsite"]}
st = [
    {"Sid": "MaintBucket", "Effect": "Allow", "Principal": m,
     "Action": ["s3:ListBucket", "s3:ListBucketVersions", "s3:GetBucketLocation", "s3:GetBucketVersioning", "s3:GetLifecycleConfiguration"],
     "Resource": [f"arn:aws:s3:::{b}"]},
    {"Sid": "MaintObjects", "Effect": "Allow", "Principal": m,
     "Action": ["s3:GetObject", "s3:GetObjectVersion", "s3:PutObject", "s3:DeleteObject"], "Resource": [f"arn:aws:s3:::{b}/repos/*"]},
    {"Sid": "MaintEscrow", "Effect": "Allow", "Principal": m, "Action": ["s3:PutObject", "s3:DeleteObject"], "Resource": [f"arn:aws:s3:::{b}/escrow/*"]},
    {"Sid": "MaintNoVersionDeletes", "Effect": "Deny", "Principal": m, "Action": ["s3:DeleteObjectVersion"], "Resource": [f"arn:aws:s3:::{b}/*"]},
]
json.dump({"Version": "2012-10-17", "Statement": st}, sys.stdout, indent=1)
print()
' "$B" >offsite-policy.json </dev/null
as holdfast-bucket-owner aws --endpoint-url "$RGW" s3api put-bucket-policy --bucket "$B" --policy file://offsite-policy.json
echo "bucket $B: versioning $(as holdfast-bucket-owner aws --endpoint-url "$RGW" s3api get-bucket-versioning --bucket "$B" --query Status --output text); policy for holdfast-offsite"
