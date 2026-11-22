# Plan for M9: rebuilding Holdfast

Status: proposed, 2026-11-22. Waiting for your answers to §5 before any product code. It was plan-m8 until decision
041 put the two-program architecture first, as M8; "M8" below reads M9.

**To be revised after M8** (decision 041): there is no vault any more, so part 2 and Q4 become "the server writes each
export to both buckets", Q9 goes, and the storage check of Q10 is on the maintenance key of each bucket. Agents are
identified by pinned certificates, not tokens, so Q10's "agents keep their tokens" becomes "the export holds the pinned
certificates, so agents reconnect by themselves".

M8 is decision 040, which decided OQ3: when holdfast-server's PostgreSQL is lost, Holdfast is rebuilt from a nightly
export in the bucket, the escrow, and a runbook. There is no standby (decision 030). The snapshot catalogue is a cache,
read again from the repositories (M2).

## 1. What M8 holds

| # | Part | From |
| --- | --- | --- |
| 1 | **The nightly export**: holdfast-server writes `meta/<date>.age` to its own bucket, its database and its config, encrypted to the escrow's age recipient | 040 |
| 2 | **Off-site**: the vault copies site A's `meta/` to site B with the rest, so nothing in A connects to B | 040, 039 |
| 3 | **Restoring an export** into a new database on a rebuilt server; agents enrolled before the export reconnect with their tokens | 040 |
| 4 | **`holdfast host import`**: a host back from its escrow file and its S3 key, when there is no export | 040 |
| 5 | **The rebuild runbook**, `docs/runbooks/rebuild-holdfast.md`, walked in the lab | 030, 040 |

Already decided, and only built here:
- the export is encrypted to `escrow.age_recipient` (decision 036); restoring it also needs the server's credentials
  key, which the admin keeps offline (`holdfast-server keygen`);
- agents keep their bearer tokens; there is no Holdfast CA;
- no hash-chained audit log: the audit log is in the export, and bucket versioning keeps the exports;
- `holdfast host import` is admin only, tests the storage contract first, and its jobs are set again by hand.

## 2. Design in brief

**1. The export.** Once a day, and on `holdfast export now`, the server writes `meta/<UTC date>.age`: a tar of each
table it needs as `COPY … TO STDOUT` CSV, with the schema version, Holdfast's version and the time, encrypted to the age
recipient. Its own format (Q1), so restoring needs no `pg_dump` of a matching version. The credentials inside stay sealed
with the credentials key, so the export alone opens no repository. The maintenance key writes it (a new storage check,
Q10). Each export is audited; a failure is a problem on the server and an alert.

**2. Off-site.** The vault lists site A's `meta/` with the read key it already has, and copies new exports to site B's
bucket under `meta/<vault's source>/`. The read key must then read `meta/*` too (Q4).

**3. Restoring.** On a rebuilt server: the packages, `server.yaml`, the credentials key put back, an empty database,
then `holdfast-server restore-export --identity <age key file> <export>`. It refuses a database that is not empty, migrates
to the export's schema, loads every table, migrates on to the running version, and audits that it was restored, from
which export. Then the server starts as usual: agents enrolled before the export reconnect with their tokens; the
catalogue is refreshed from the repositories; what was lost is shown (Q6).

**4. `holdfast host import <fqdn> --escrow-file <decrypted> --s3-key-file <key>`**: for a host the export does not have,
or with no export at all. It checks that the file is that host's, tests the key against the storage contract, proves the
password opens the repository (`restic cat config`), and adds the host with that repository and password, and an
enrollment code. No job: the admin sets them again, from the config in the export if there is one.

**5. The runbook**, `docs/runbooks/rebuild-holdfast.md`: what to have offline (the age key, the credentials key, the
packages), the steps in order, how to check each one, and what is lost (Q6).

## 3. Tests

- **e2e:** a nightly export written, and copied by a vault; a fresh database restored from it, and the server started on
  it: agents reconnect, the config, history and audit log are back, the catalogue refreshes; a refused restore into a
  database that is not empty; an agent newer than the export (Q6); `host import` from an escrow file, its storage test
  and its password check.
- **Lab:** the runbook walked on hfsrv: its database dropped and `/etc/holdfast` and `/var/lib/holdfast-server` removed,
  the packages installed again, the export restored, the three agents back without new codes (Q8).

## 4. Order

The export first, then restoring it, then `host import`, then the vault's copy of `meta/`, then the runbook and the lab.

## 5. Questions

**Q1. The export's format.** Decision 040 says "a PostgreSQL dump".
- *Proposal:* Holdfast's own: each table as `COPY … TO STDOUT` CSV in a tar, with the schema version, restored by
  `holdfast-server restore-export`. It needs no PostgreSQL client tools on the server, works whatever PostgreSQL version
  the rebuilt server has, and loads into an older schema before migrating forward.
- *The alternative:* `pg_dump --format=custom`, restored with `pg_restore`. Standard, but it needs the client tools at a
  version no older than the server's, and a dump from a newer PostgreSQL may not load into an older one.

**Q2. What is in it.**
- *Proposal:* every table except the sessions (they would be stale, and a rebuilt server's users log in again) and a
  vault's cached feed (it is fetched again). The snapshot catalogue is in it: it is a cache, but its tombstones (what
  Holdfast deleted, and when; what was deleted on request; what was acknowledged) cannot be read again from a
  repository, and without them the "disappeared" and "Deleted in primary" views would be wrong after a rebuild.
- *The alternative:* leave the catalogue out, and read it all again; tombstones are lost.

**Q3. When, and how many are kept.**
- *Proposal:* once a day, at 00:30 in the server's default zone, and at start when the last one is older than a day;
  `holdfast export now` for an admin (API, CLI, UI). The server keeps the newest 30 and deletes older ones with the
  maintenance key; bucket versioning then keeps them for the lifecycle rule's days. `holdfast_export_last_success_
  timestamp_seconds` and `HoldfastExportStale` after 2 days. Without an age recipient no export is written, and the
  server warns, as for escrow.
- *The alternative:* the admin's lifecycle rule expires `meta/` (an expiration rule, besides the noncurrent one), and
  Holdfast deletes nothing.

**Q4. The vault's copy of `meta/`.**
- *Proposal:* through site A's bucket, with the vault's read key, which then reads `meta/*` as well as `repos/*`
  (docs/storage.md changes; `holdfast vault add` tests it). Into site B's bucket under `meta/<vault name>/`, kept as
  site A keeps them. It works while site A's server is down.
- *The alternative:* through the vault protocol (`/vault/v1/export`): no change to the key, but no copy while site A's
  server is down.

**Q5. Restoring.**
- *Proposal:* `holdfast-server restore-export` (on the server, as root; not an API: there is no server yet), into an empty
  database only. The vault restores its own exports the same way (`holdfast-vault restore-export`).
- *Open detail:* the age identity on a file given on the command line, read once and never kept. *Proposal:* yes.

**Q6. What a rebuilt server lacks, and agents ahead of it.** Revisions, history and audit entries after the export are
lost. An agent may run a revision newer than the restored head.
- *Proposal:* the server sends no config to an agent that reports a revision newer than its head, and shows the problem
  "runs revision N, newer than this server's head M: make the change again"; the agent keeps running its own config
  meanwhile. The first save that touches the host sends it as usual. Agents enrolled after the export get 401, keep
  backing up, and are brought back with `holdfast host import` (or `host enroll-code` if the export has the host).
- *The alternative:* send the restored config to every agent at once, which could quietly undo a change made after the
  export, a job added included.

**Q7. `host import` in the UI.** UI = API = CLI.
- *Proposal:* all three: `POST /api/v1/hosts/import`, admin everywhere, with the escrow file's contents and the S3 key
  as input (never shown back), and a form in the UI beside Add host.
- *The alternative:* CLI and API only, listed in spec §3.3 as an exception, since a repository password then goes
  through a browser.

**Q8. The lab's walk.**
- *Proposal:* on hfsrv itself: its database dropped, `/etc/holdfast` and the server's state removed, the packages
  removed and installed again, the credentials key and `server.yaml` put back from the lab's state, the export restored;
  then the three agents back by themselves. It needs no new VM.
- *The alternative:* a new server VM, which the lab would have to make (vms.yml, a sixth VM).

**Q9. The vault's own export.** The vault is holdfast-server, so it writes its export to site B's bucket by itself,
which rebuilds a lost vault the same way. *Proposal:* yes, the same code and settings; nothing to build.

**Q10. The storage contract.** *Proposal:* a new check, B016: the maintenance key may write, read and delete under
`meta/`; host keys may not read `meta/` (B007 already tests another prefix: it gains `meta/`).
