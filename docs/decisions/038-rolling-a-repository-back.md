# 038: Rolling a repository back from S3's versions

Status: accepted, 2026-11-22.

## Decision

- An admin (`repository.rollback`) can roll one host's whole prefix back to a point in time, after a preview, by
  typing the FQDN.
- It uses only copies and delete markers: the version current at that time is copied over today's, and an object
  that was not there then gets a delete marker. **No version is ever deleted**, so a rollback can itself be rolled
  back, until the bucket's lifecycle rule expires the old versions.
- `locks/` is left alone; everything else is rolled back, `keys/` and `config` included.
- It is refused for a time before the host's last password rotation (the server no longer has that password),
  and for a time older than the lifecycle rule's days.
- The host's agent is paused and idle while it runs; `restic check` runs afterwards, and the snapshot list is read
  again.
- **It is a third exception to the 7-day protection,** beside Prune now and Delete now, and CLAUDE.md says so.

## Why

- **A recovery tool that waits 7 days does not recover anything.** A rollback is for a repository damaged or
  emptied behind Holdfast's back (decision 032), or after a mistaken Prune now.
- **It deletes nothing for good.** What it sets aside stays in S3 as old versions, and rolling the rollback back
  brings it back. That is the same floor every other deletion stands on (decision 030 §14).
- **The whole prefix, to one time,** because a repository at any one moment is what a crash at that moment would
  have left, which restic is built to survive. Rolling back only some files could mix moments.
- The decision 013 "no rollback" is about config revisions, and stands.
