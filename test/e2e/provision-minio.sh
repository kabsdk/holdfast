#!/bin/bash
# Storage provisioner for MinIO, used by the e2e tests (holdfast-server's storage.provisioner_command).
#
# holdfast-server passes HOLDFAST_FQDN, HOLDFAST_BUCKET and HOLDFAST_PREFIX in the environment and the
# host's access key and secret on stdin. This makes the MinIO user exist with the host policy
# (spec §2.3): read, write and list under the host's prefix only, and delete only under locks/.
# It is safe to run again. It needs MC (path to mc) and MC_HOST_hf (the admin alias URL).
#
# Test-only: mc takes the secret as an argument, so it is briefly visible in the process list.
set -euo pipefail
ak="" sk=""
while IFS='=' read -r k v; do
  case "$k" in
    ACCESS_KEY_ID) ak=$v ;;
    SECRET_ACCESS_KEY) sk=$v ;;
  esac
done
[ -n "$ak" ] && [ -n "$sk" ] || { echo "provision-minio: access key and secret expected on stdin" >&2; exit 2; }
mc=${MC:-mc}
b=$HOLDFAST_BUCKET p=$HOLDFAST_PREFIX
policy=$(mktemp)
trap 'rm -f "$policy"' EXIT
cat > "$policy" <<EOF
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::$b"],"Condition":{"StringLike":{"s3:prefix":["$p","$p/*"]}}},
 {"Effect":"Allow","Action":["s3:GetBucketLocation"],"Resource":["arn:aws:s3:::$b"]},
 {"Effect":"Allow","Action":["s3:GetObject","s3:PutObject"],"Resource":["arn:aws:s3:::$b/$p/*"]},
 {"Effect":"Allow","Action":["s3:DeleteObject"],"Resource":["arn:aws:s3:::$b/$p/locks/*"]}]}
EOF
name="holdfast-$b-$(printf '%s' "$HOLDFAST_FQDN" | tr '.' '-')"
"$mc" admin user add hf "$ak" "$sk" >/dev/null
"$mc" admin policy create hf "$name" "$policy" >/dev/null
"$mc" admin policy attach hf "$name" --user "$ak" >/dev/null 2>&1 || true # already attached
