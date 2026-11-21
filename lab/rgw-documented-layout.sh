#!/usr/bin/env bash
# lab/rgw-documented-layout.sh: rebuild the lab bucket `holdfast` on s3a in the layout docs/storage.md
# describes, so Holdfast's contract test (B001-B011) can pass against real Ceph RGW. Run it on s3a as root:
#
#   lab/lab ssh m1 s3a 'bash -s' < lab/rgw-documented-layout.sh
#
# It DELETES the bucket and everything in it (lab data only), then makes it again:
#   - owned by `holdfast-bucket-owner`, an identity of the admin's own, not by the maintenance key (B010);
#   - versioning on (B008), and a lifecycle rule that expires noncurrent versions under repos/ (B011);
#   - a bucket policy: the maintenance user `holdfast-maint` may read, write, list and delete under repos/,
#     including what host users wrote (B003, B004), and is denied version deletes (B009); the host user
#     `test-host` may use repos/test.example.com/ only, and delete only under its locks/ (B001-B007).
# Plain RGW users, no account: the bucket-policy grant is what lets the maintenance user read the host's
# objects (docs/storage.md, "If B003 fails").
set -euo pipefail
cd /root/hf-lab
RGW=http://s3a.m1.holdfast.test
B=holdfast
as() { local u=$1; shift; (set -a; . "/root/hf-lab/$u.env"; set +a; "$@"); }

if ! radosgw-admin user info --uid holdfast-bucket-owner >/dev/null 2>&1; then
	radosgw-admin user create --uid holdfast-bucket-owner --display-name "Holdfast bucket owner (the admin's identity)" >/dev/null
fi
(umask 077; radosgw-admin user info --uid holdfast-bucket-owner | python3 -c 'import json, sys
k = json.load(sys.stdin)["keys"][0]
print("AWS_ACCESS_KEY_ID=" + k["access_key"]); print("AWS_SECRET_ACCESS_KEY=" + k["secret_key"])' >holdfast-bucket-owner.env)

echo "removing bucket $B and its data (lab only)"
radosgw-admin bucket rm --bucket="$B" --purge-objects --bypass-gc >/dev/null 2>&1 || true
as holdfast-bucket-owner aws --endpoint-url "$RGW" s3api create-bucket --bucket "$B" >/dev/null
as holdfast-bucket-owner aws --endpoint-url "$RGW" s3api put-bucket-versioning --bucket "$B" --versioning-configuration Status=Enabled

cat >lifecycle.json <<'EOF'
{"Rules": [{"ID": "expire-noncurrent", "Status": "Enabled", "Filter": {"Prefix": "repos/"},
  "NoncurrentVersionExpiration": {"NoncurrentDays": 30}}]}
EOF
as holdfast-bucket-owner aws --endpoint-url "$RGW" s3api put-bucket-lifecycle-configuration --bucket "$B" --lifecycle-configuration file://lifecycle.json

M='{"AWS": ["arn:aws:iam:::user/holdfast-maint"]}'
H='{"AWS": ["arn:aws:iam:::user/test-host"]}'
P=repos/test.example.com
cat >documented-policy.json <<EOF
{"Version": "2012-10-17", "Statement": [
  {"Sid": "MaintBucket", "Effect": "Allow", "Principal": $M,
   "Action": ["s3:ListBucket", "s3:GetBucketLocation", "s3:GetBucketVersioning", "s3:GetLifecycleConfiguration"],
   "Resource": ["arn:aws:s3:::$B"]},
  {"Sid": "MaintObjects", "Effect": "Allow", "Principal": $M,
   "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"], "Resource": ["arn:aws:s3:::$B/repos/*"]},
  {"Sid": "MaintNoVersionDeletes", "Effect": "Deny", "Principal": $M,
   "Action": ["s3:DeleteObjectVersion"], "Resource": ["arn:aws:s3:::$B/*"]},
  {"Sid": "HostLocation", "Effect": "Allow", "Principal": $H, "Action": ["s3:GetBucketLocation"], "Resource": ["arn:aws:s3:::$B"]},
  {"Sid": "HostList", "Effect": "Allow", "Principal": $H, "Action": ["s3:ListBucket"], "Resource": ["arn:aws:s3:::$B"],
   "Condition": {"StringLike": {"s3:prefix": ["$P", "$P/*"]}}},
  {"Sid": "HostObjects", "Effect": "Allow", "Principal": $H, "Action": ["s3:GetObject", "s3:PutObject"], "Resource": ["arn:aws:s3:::$B/$P/*"]},
  {"Sid": "HostLocks", "Effect": "Allow", "Principal": $H, "Action": ["s3:DeleteObject"], "Resource": ["arn:aws:s3:::$B/$P/locks/*"]}
]}
EOF
as holdfast-bucket-owner aws --endpoint-url "$RGW" s3api put-bucket-policy --bucket "$B" --policy file://documented-policy.json
echo "bucket $B: owner $(radosgw-admin bucket stats --bucket "$B" | python3 -c 'import json,sys; print(json.load(sys.stdin)["owner"])'), versioning $(as holdfast-bucket-owner aws --endpoint-url "$RGW" s3api get-bucket-versioning --bucket "$B" --query Status --output text)"
