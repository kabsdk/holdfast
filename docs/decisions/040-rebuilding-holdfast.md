# 040: Rebuilding Holdfast

Status: accepted, 2026-11-22 (plan-m7 Q9–Q12; you answered "Continue"). It decides OQ3. It is built in M9
([plan-m9](../plan-m9.md)): decision 041 put the two-program architecture first, as M8. With no vault, the server
writes each export to both buckets itself; and agents are known by pinned certificates, not tokens, which the export
holds just the same. As built (plan-m9 Q1): the export is the tables as CSV, not a PostgreSQL dump, and "the config"
is the config revisions in the database: `server.yaml` is not in it, as it may hold the database's password, so the
admin keeps it offline with its key files (docs/runbooks/rebuild-holdfast.md).

## Decision

- **A nightly export**: holdfast-server writes `meta/<date>.age` to its own bucket, a PostgreSQL dump and the config,
  encrypted to the escrow's age recipient (decision 036). The vault copies `meta/` to the other site. Nothing in A
  connects to B. Restoring it also needs the server's credentials key, which the admin keeps offline.
- **Agents keep their bearer tokens**: the export holds their hashes, so agents enrolled before it reconnect by
  themselves; one enrolled after it gets a new code. No Holdfast CA.
- **Without any export**, `holdfast host import <fqdn> --escrow-file … --s3-key-file …` (admin) adds a host back with
  its existing repository and password, after the storage test.
- **No hash-chained audit log**: the audit log is in the nightly export, which bucket versioning keeps.
- The snapshot catalogue stays a cache, read again from the repositories.

## Why

- **The export is the smallest thing that makes a lost database survivable**, and the escrow already covers each
  repository on its own. OQ3's `holdfast-meta` bucket with a hash-chained log, and a CA, are more complete and much
  more work; they remain possible later.
