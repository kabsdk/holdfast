# 029: The admin makes the S3 keys; Holdfast tests the storage contract

Status: accepted, 2026-11-21. Answers plan-m1 Q4's first question.

## Decision

- **Holdfast never creates S3 users, keys or policies.** The admin makes them with whatever their S3
  server offers:
  - the bucket;
  - the server's key, called the maintenance key;
  - one key per host, given to Holdfast when the host is added. It can be replaced later.
- **Holdfast states what each key must and must not be allowed to do, in plain S3 terms**
  ([storage.md](../storage.md)).
- **Holdfast tests that contract itself, using ordinary S3 calls,** with the host's key and the maintenance
  key. It does so:
  - when a host is added;
  - when a host's key is replaced;
  - before an enrollment is accepted;
  - in each host's daily check.

  Each failure is a storage check with a stable code (spec §4.5). While any check fails, the host cannot
  enroll.
- **An optional provisioner command stays,** for admins who want key creation automated. Holdfast then
  generates the key and runs the command. Holdfast ships no vendor-specific provisioner.
- **One key per host stays** (spec §2.3).

## Why

- **There is no standard API for creating S3 identities.** AWS IAM, MinIO's admin API, Ceph RGW's admin
  API and RGW accounts all differ, and so do other providers. Reading and writing objects is the standard
  part, and it is all restic needs. Building in user creation would tie Holdfast to vendors.
- **Documentation alone is not enough, because the failures are silent.** The lab showed this on Ceph RGW.
  There, an object belongs to the user who uploaded it. With plain users, the maintenance key cannot read
  any host's data, so check fails, and in M3 forget and prune would fail too. Backups still succeed, so
  nothing looks wrong until maintenance runs. MinIO has no ownership per object, so the CI suite could not
  show it.

  A contract tested with plain S3 calls catches this, and wrong host policies too, at the moment a host is
  added. It names the property that is missing, whichever S3 server is behind it.
- **One key per host limits a compromised host to its own prefix.** With a shared key, a compromised host
  could overwrite other hosts' current objects. Versioning keeps the old copies, but restoring them would
  take work.

## Consequences

- `holdfast host add` takes `--s3-key-file`. The key is required unless a provisioner command is
  configured. `holdfast host set-key` replaces a host's key. Both are API endpoints first (decision 028).
  The UI gets the same field and action in M4.
- The key passes into the API as input only. It is never in any output, log or revision.
- The contract test writes a few tiny probe objects under the host's prefix and deletes them again. With
  versioning on, their old versions stay until the bucket's lifecycle rule expires them (Q4's third
  question, still open).
- The vault's reader key (off-site O10) will follow the same pattern.
