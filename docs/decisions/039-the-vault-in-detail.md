# 039: The vault in detail

> **Superseded in part by [decision 041](041-two-programs-gateway-and-agent-certificates.md) (M8):** the vault is now
> the server's off-site instance, in the same program and database, linked in-process: no registration, token, rclone,
> read key or restore key. A restore from off-site reads the copy through the gateway. The rest of this record stands.

Status: accepted, 2026-11-22. It decides the vault's remaining open points, on top of decision 027.

## Decision

- **Each site runs its own holdfast-server for its own hosts, and its own vault for the other site's copies.**
  When the sites are cut off, each keeps backing up at home, and copies catch up afterwards.
- **The vault reads site A through restic's `rclone:` backend**, with a read-only key for A's `repos/` in rclone's
  environment, and writes site B through `s3:` with its own key. The vault's package ships a pinned rclone.
- **Restores from site B**: B's storage admin makes one read-only key per copied host, for its B prefix, and
  gives it to the vault. holdfast-server asks; the vault answers with B's repository, its password and that key; the
  restoring agent gets them sealed, once. A grant lasts as long as the restore command, 24 hours.
- **Off-site shorter than primary** is a warning, O101, that the vault admin accepts.
- **Roles in the vault**: admin and viewer, by default `holdfast-vault-admins` and `holdfast-vault-viewers`.
- **The off-site lag alerts** when a job's newest copy is more than twice the job's longest gap behind its newest
  backup.
- **The hold**: site A's daily run does not forget a snapshot younger than 7 days that a vault still wants.
- **Site B's storage** passes the same storage checks as site A's; versioning and a lifecycle rule are required, the
  days are B's storage admin's choice, and there is no Object Lock. A separate cluster, or realm, is recommended, not
  tested.
- **Logs** go to the journal, and from there to syslog where a site has one; the audit log stays in the vault's own
  database. The vault has a PostgreSQL of its own.
- The decommission's wait (7 days) and no standby (B's escrow and plain restic) were decided before.

## Why

- **One key for both repositories of a copy would cross the sites.** restic 0.19.1's `copy` takes a separate password
  and repository for the source, but not separate S3 keys. The same key on both S3 servers would put a secret that can
  write to B in A's S3 user database; rclone keeps A's key read-only, and in B alone.
- **Symmetry is the simplest answer to "where do B's hosts go"**: no host backs up across the link, and each site's
  backups keep going when it is cut.
- **A hold, but a bounded one** (decision 030 §12): a stalled vault must not stop A's pruning for ever.
