# Storage: what Holdfast needs from S3

Holdfast works with any S3 server. It never creates S3 users, keys or policies itself (decision 029). You
make the keys with your S3 server's own tools. Holdfast then tests every rule below with plain S3 calls
when a host is added, when its key is replaced, before an enrollment is accepted, and in each host's daily
check. Any rule that fails is reported with its code (spec §4.5), and the host cannot enroll until it is
fixed.

## One bucket, with versioning on

- Each host's repository lives under `repos/<fqdn>/` in that bucket.
- **Versioning must be enabled (B008).** It is what makes a deleted or overwritten object recoverable.
- Set a lifecycle rule to expire noncurrent versions, or the bucket only ever grows. The expiry is also how
  long deleted data can still be recovered.

## The keys

| Key | Must be able to | Must not be able to |
| --- | --- | --- |
| **Host key**, one per host, for `repos/<fqdn>/` | Read, write and list under `repos/<fqdn>/` (B001, B002). Delete under `repos/<fqdn>/locks/` only. | Delete anything else under its prefix (B005). Delete, for good, a version of an object outside `locks/` (B006). Read, write or list any other host's prefix, or list `repos/` itself (B007). |
| **Maintenance key**, the server's: `storage.maintenance_credentials_file` | Read, write, list and delete everything under `repos/`, **including objects that host keys wrote** (B003, B004). Read the bucket's versioning setting. | |
| **Witness key**, for `holdfast-freshness` | List `repos/`. | Anything else. |

## Example host policy

This uses the standard S3 policy language, which AWS, MinIO and Ceph RGW all accept. Replace `holdfast`
and `web01.example.com` with your bucket and the host's FQDN.

```json
{"Version": "2012-10-17", "Statement": [
  {"Effect": "Allow", "Action": ["s3:GetBucketLocation"], "Resource": ["arn:aws:s3:::holdfast"]},
  {"Effect": "Allow", "Action": ["s3:ListBucket"], "Resource": ["arn:aws:s3:::holdfast"],
   "Condition": {"StringLike": {"s3:prefix": ["repos/web01.example.com", "repos/web01.example.com/*"]}}},
  {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject"],
   "Resource": ["arn:aws:s3:::holdfast/repos/web01.example.com/*"]},
  {"Effect": "Allow", "Action": ["s3:DeleteObject"],
   "Resource": ["arn:aws:s3:::holdfast/repos/web01.example.com/locks/*"]}
]}
```

`s3:GetBucketLocation` is there because restic on a host first asks the bucket for its region.

Some servers read the policy language differently, which is why Holdfast tests the policy instead of
trusting it. On MinIO, for example, `s3:DeleteObject` also deletes object versions for good. A broader
delete grant than the one above therefore fails B006 there as well as B005.

## If B003 fails: who owns an object

On some S3 servers, an object belongs to the user who uploaded it, and other users cannot read it, even the
bucket's owner. Ceph RGW without accounts works like this. So do AWS buckets that predate S3 Object Ownership.
restic cannot ask for the bucket owner to get access, so the fix is on the storage side:

- **Put all of Holdfast's users in one account** (on Ceph RGW: an RGW account, Squid or later). Objects then
  belong to the account, and each user's policy decides access.
- **Or add a bucket-policy statement** that gives the maintenance user `s3:*` on the bucket and on
  `repos/*`.

Both were verified against Ceph Squid 19.2.3 in the lab (`lab/README.md`).

## Adding a host

```
holdfast host add web01.example.com --s3-key-file /root/web01.env --job system --path /etc --daily 02:00
```

The key file holds `AWS_ACCESS_KEY_ID=` and `AWS_SECRET_ACCESS_KEY=` lines, and must be readable by root
only. Use `-` to read it from standard input. To replace the key later:

```
holdfast host set-key web01.example.com --s3-key-file /root/web01-new.env
```

The agent picks up the new key on its next poll.

If `storage.provisioner_command` is set in `server.yaml`, `--s3-key-file` can be left out. Holdfast then
generates the key and runs that command to create it (see `internal/server/storage`).
