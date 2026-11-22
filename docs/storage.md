# Storage: what Holdfast needs from S3

Holdfast works with any S3 server. It never creates S3 users, keys or policies itself (decision 029). You
make the keys with your S3 server's own tools. Holdfast tests every rule below with plain S3 calls:

- when a host is added;
- when its key is replaced;
- before an enrollment is accepted;
- in each host's daily check.

A rule that fails is reported with its code (spec §4.5). The host cannot enroll until the rule is fixed.

## One bucket, with versioning and a lifecycle rule

- **Make the bucket with an identity of your own, not with the maintenance key.** A bucket's owner can
  change its settings, so a maintenance key that owns the bucket fails B010.
- Each host's repository lives under `repos/<fqdn>/` in that bucket. With `escrow.age_recipient` set, each
  host's repository password is also written, encrypted, to `escrow/<fqdn>.age` (decision 036).
- **Versioning must be enabled (B008).** It is what makes a deleted or overwritten object recoverable.
- **A lifecycle rule must expire noncurrent versions under `repos/` (B011).** Without one, the bucket only
  grows: nothing a prune deletes is ever freed. The expiry is your choice. It is also how long deleted
  data can still be recovered, and Holdfast uses it to say when deleted data is really gone: the prune's
  date plus the rule's days (spec §3.4). With several rules, it counts the longest. **Let the rule cover
  the whole bucket**, so it also expires the old versions of the escrow files and of the storage test's
  small probe objects under `escrow/`.
- **The real safety floor is versioning plus that expiry.** It holds against a compromised host, a
  compromised server and Prune now, as long as no Holdfast key can delete versions or change the bucket's
  settings (B006, B009, B010).

## The keys

| Key | Must be able to | Must not be able to |
| --- | --- | --- |
| **Host key**, one per host, for `repos/<fqdn>/` | Read, write and list under `repos/<fqdn>/` (B001, B002). Delete under `repos/<fqdn>/locks/` only. | Delete anything else under its prefix (B005). Delete, for good, a version of an object outside `locks/` (B006). Read, write or list any other host's prefix, or list `repos/` itself (B007). |
| **Maintenance key**, the server's: `storage.maintenance_credentials_file` | Read, write, list and delete everything under `repos/`, **including objects that host keys wrote** (B003, B004). Write and delete under `escrow/`, when an escrow recipient is set (B012). List object versions, for the repository's size. Read the bucket's versioning setting and lifecycle rule. | Delete an object version for good (B009). Change the bucket's settings, such as versioning, the lifecycle rule or the bucket policy (B010). |
| **Witness key**, for `holdfast-freshness` | List `repos/`. | Anything else. |

## Example policies

These use the standard S3 policy language, which AWS, MinIO and Ceph RGW all accept. Replace `holdfast`
and `web01.example.com` with your bucket and the host's FQDN.

**Host key:**

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

**Maintenance key:**

```json
{"Version": "2012-10-17", "Statement": [
  {"Effect": "Allow", "Action": ["s3:ListBucket", "s3:ListBucketVersions", "s3:GetBucketLocation",
   "s3:GetBucketVersioning", "s3:GetLifecycleConfiguration"], "Resource": ["arn:aws:s3:::holdfast"]},
  {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"],
   "Resource": ["arn:aws:s3:::holdfast/repos/*"]},
  {"Effect": "Allow", "Action": ["s3:PutObject", "s3:DeleteObject"],
   "Resource": ["arn:aws:s3:::holdfast/escrow/*"]},
  {"Effect": "Deny", "Action": ["s3:DeleteObjectVersion"], "Resource": ["arn:aws:s3:::holdfast/*"]}
]}
```

`s3:GetBucketLocation` is there because restic first asks the bucket for its region. `s3:ListBucketVersions`
lets the server measure each repository with its old versions, once a day; without it the host shows the
`size` warning (AWS and Ceph RGW need it; MinIO lists versions with `s3:ListBucket`). The maintenance key
writes the escrow files, but does not need to read them: it could not decrypt them anyway. Deleting one
happens only at decommission, and leaves its old versions, as everywhere else.

**Keep the explicit deny.** Some servers read the policy language differently, which is why Holdfast tests
the policy instead of trusting it. On MinIO, `s3:DeleteObject` on its own also deletes versions for good,
and only the explicit deny stops that (checked against MinIO). So:

- a host grant broader than `locks/*` fails B006 there as well as B005;
- a maintenance key without the deny fails B009.

## If B003 fails: who owns an object

On some S3 servers, an object belongs to the user who uploaded it, and other users cannot read it, even the
bucket's owner. Ceph RGW without accounts works like this, and so do AWS buckets that predate S3 Object
Ownership. restic cannot ask for the bucket owner to get access, so the fix is on the storage side:

- **Put all of Holdfast's users in one account.** On Ceph RGW that is an RGW account, from Squid on.
  Objects then belong to the account, and each user's policy decides access.
- **Or add a bucket-policy statement** that gives the maintenance user the maintenance key's object actions
  on `repos/*`: `s3:GetObject`, `s3:PutObject` and `s3:DeleteObject`, with the deny above. Do not grant
  `s3:*`. That would fail B009 and B010.

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

The agent picks up the new key on its next poll, and each run after that uses it. **Remove the old key only
after a backup has run with the new one.** `holdfast host show` gives the agent's credentials version, and
`holdfast host events` the runs. A run that is still using the old key fails if its key disappears.

If `storage.provisioner_command` is set in `server.yaml`, `--s3-key-file` can be left out. Holdfast then
generates the key and runs that command to create it (see `internal/server/storage`).
