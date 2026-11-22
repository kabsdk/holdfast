# Storage: what Holdfast needs from S3

Holdfast works with any S3 server. It never creates S3 users, keys or policies itself (decision 029). You make
**two keys per bucket** with your S3 server's own tools, and give them to holdfast-server. Hosts hold no S3 key:
they back up through the server's gateway, which uses the gateway key for all of them (decision 041).

Holdfast tests every rule below with plain S3 calls:

- when a host is added;
- before an enrollment is accepted;
- in each host's daily check, and in `holdfast check`.

A rule that fails is reported with its code (spec §4.5). The host cannot be added or enroll until the rule is fixed.

## One bucket, with versioning and a lifecycle rule

- **Make the bucket with an identity of your own, not with either key.** A bucket's owner can change its
  settings, so a maintenance key that owns the bucket fails B010.
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
- Object Lock is not required. If the bucket has it, and the maintenance key may read the setting
  (`s3:GetBucketObjectLockConfiguration`), `holdfast host show` reports it. Holdfast never extends a lock.

## The keys

| Key | Must be able to | Must not be able to |
| --- | --- | --- |
| **Gateway key**, the server's: `storage.gateway_credentials_file` | Read, write and list under `repos/` (B001, B002). Delete lock files, under `repos/<fqdn>/locks/`. | Delete anything else (B005). Delete a version for good (B006). Use the bucket outside `repos/`, such as the escrow files (B007). |
| **Maintenance key**, the server's: `storage.maintenance_credentials_file` | Read, write, list and delete everything under `repos/`, **including objects the gateway key wrote** (B003, B004). Write and delete under `escrow/`, when an escrow recipient is set (B012). List object versions, for the repository's size, and read them, for a rollback. Read the bucket's versioning setting and lifecycle rule. | Delete an object version for good (B009). Change the bucket's settings, such as versioning, the lifecycle rule or the bucket policy (B010). |
| **Witness key**, for `holdfast-freshness` | List `repos/`. | Anything else. |

The gateway key may write every host's repository; the gateway decides which host may use which (spec §2.3): a
host reads and adds to its own repository, deletes only its own lock files, and never rewrites a file. The
key's limits are what holds if the server itself is compromised: it still cannot delete a snapshot for good.

## Example policies

These use the standard S3 policy language, which AWS, MinIO and Ceph RGW all accept. Replace `holdfast` with your
bucket.

**Gateway key:**

```json
{"Version": "2012-10-17", "Statement": [
  {"Effect": "Allow", "Action": ["s3:ListBucket", "s3:GetBucketLocation"], "Resource": ["arn:aws:s3:::holdfast"]},
  {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject"], "Resource": ["arn:aws:s3:::holdfast/repos/*"]},
  {"Effect": "Allow", "Action": ["s3:DeleteObject"], "Resource": ["arn:aws:s3:::holdfast/repos/*/locks/*"]},
  {"Effect": "Deny", "Action": ["s3:DeleteObjectVersion"], "Resource": ["arn:aws:s3:::holdfast/*"]}
]}
```

**Maintenance key:**

```json
{"Version": "2012-10-17", "Statement": [
  {"Effect": "Allow", "Action": ["s3:ListBucket", "s3:ListBucketVersions", "s3:GetBucketLocation",
   "s3:GetBucketVersioning", "s3:GetLifecycleConfiguration"], "Resource": ["arn:aws:s3:::holdfast"]},
  {"Effect": "Allow", "Action": ["s3:GetObject", "s3:GetObjectVersion", "s3:PutObject", "s3:DeleteObject"],
   "Resource": ["arn:aws:s3:::holdfast/repos/*"]},
  {"Effect": "Allow", "Action": ["s3:PutObject", "s3:DeleteObject"],
   "Resource": ["arn:aws:s3:::holdfast/escrow/*"]},
  {"Effect": "Deny", "Action": ["s3:DeleteObjectVersion"], "Resource": ["arn:aws:s3:::holdfast/*"]}
]}
```

`s3:GetBucketLocation` is there because restic first asks the bucket for its region. `s3:ListBucketVersions`
lets the server measure each repository with its old versions, once a day; without it the host shows the
`size` warning (AWS and Ceph RGW need it; MinIO lists versions with `s3:ListBucket`). `s3:GetObjectVersion`
lets a rollback copy an old version over the current one (spec §3.5b); a rollback's preview tests it, the daily
storage test does not. The maintenance key writes the escrow files, but does not need to read them: it could not
decrypt them anyway. Deleting one happens only at decommission, and leaves its old versions, as everywhere else.

**Keep the explicit denies.** Some servers read the policy language differently, which is why Holdfast tests
the policy instead of trusting it. On MinIO, `s3:DeleteObject` on its own also deletes versions for good,
and only the explicit deny stops that (checked against MinIO). So:

- a gateway grant broader than `locks/*` fails B006 there as well as B005;
- a key without the deny fails B006 or B009.

The wildcard in the middle of `repos/*/locks/*` holds on MinIO and on Ceph RGW (checked in the lab).

## A restore onto another host

To restore host A's snapshot onto host B (spec §5.4), no key is needed: the server lets B's certificate read A's
repository through the gateway for that one restore, and only read it. The grant ends when B reports the restore.

## The off-site bucket (optional, decision 041)

Off-site copies are optional (spec §3.7). With `offsite:` in `server.yaml`, the server copies every host's snapshots
into a second bucket, best in another site: another cluster, or at least another realm. Holdfast cannot test that.

- **Make it as the primary bucket**: with versioning and a lifecycle rule, and with an identity of your own.
- **One key**: `offsite.maintenance_credentials_file`, with the maintenance key's policy above, for the off-site
  bucket. The server copies into it, forgets and prunes there behind the 7-day protection, and writes the off-site
  passwords' escrow files there. It tests B004 and B008–B012 with it, as for the primary bucket.
- **Use a key of its own**, not one of the primary bucket's: a key that can write both buckets would put both under
  one secret.
- No host ever has this key. A restore from off-site reads the copy through the server's gateway.
- The copier reads the primary repositories from the server itself, read-only, so it needs no primary key of its own.

## If B003 fails: who owns an object

On some S3 servers, an object belongs to the user who uploaded it, and other users cannot read it, even the
bucket's owner. Ceph RGW without accounts works like this, and so do AWS buckets that predate S3 Object
Ownership. restic cannot ask for the bucket owner to get access, so the fix is on the storage side:

- **Put both of Holdfast's users in one account.** On Ceph RGW that is an RGW account, from Squid on.
  Objects then belong to the account, and each user's policy decides access.
- **Or add a bucket-policy statement** that gives the maintenance user the maintenance key's object actions
  on `repos/*`: `s3:GetObject`, `s3:PutObject` and `s3:DeleteObject`, with the deny above. Do not grant
  `s3:*`. That would fail B009 and B010.

Both were verified against Ceph Squid 19.2.3 in the lab (`lab/README.md`).

## Adding a host

```
holdfast host add web01.example.com --job system --path /etc --daily 02:00
```

Nothing to make at the S3 server: the host's storage is tested with the gateway key on `repos/web01.example.com/`,
and the host is added once it passes. The host enrolls with the code it is given, and makes its own key and
certificate then (spec §5.7).
