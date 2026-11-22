# Plan for M9: rebuilding Holdfast

Status: proposed, 2026-11-22; revised after M8 for decision 041 (one server program, a gateway, agents identified by
pinned certificates, off-site copies made by the server and optional). Waiting for your answers to §5 before any
product code.

M9 is decision 040, which decided OQ3: when holdfast-server's PostgreSQL is lost, Holdfast is rebuilt from a nightly
export in the bucket, the escrow, and a runbook. There is no standby (decision 030). The snapshot catalogue is a cache,
read again from the repositories (M2).

## 1. What M9 holds

| # | Part | From |
| --- | --- | --- |
| 1 | **The nightly export**: holdfast-server writes `meta/<date>.age` to its bucket: its database (both schemas, primary's and off-site's) and its config, encrypted to the escrow's age recipient | 040 |
| 2 | **Off-site, when `offsite:` is set**: the server writes the same export to the off-site bucket too. Without `offsite:`, nothing about it is shown or checked | 040, 041 |
| 3 | **Restoring an export** into a new database on a rebuilt server; agents enrolled before the export reconnect by themselves, as the export holds their certificates' pins | 040, 041 |
| 4 | **`holdfast host import`**: a host back from its escrow file, when there is no export | 040 |
| 5 | **The rebuild runbook**, `docs/runbooks/rebuild-holdfast.md`, walked in the lab | 030, 040 |

Already decided, and only built here:
- the export is encrypted to `escrow.age_recipient` (decision 036); restoring it also needs the server's credentials
  key, which the admin keeps offline (`holdfast-server keygen`);
- agents are identified by the pinned fingerprint of their own certificate (decision 041); there is no Holdfast CA;
- no hash-chained audit log: the audit log is in the export, and bucket versioning keeps the exports;
- `holdfast host import` is admin only, and its jobs are set again by hand.

## 2. Design in brief

**1. The export.** Once a day, and on `holdfast export now`, the server writes `meta/<UTC date>.age`: a tar of each
table it needs, in both schemas, as `COPY … TO STDOUT` CSV, with the schema version, Holdfast's version and the time,
encrypted to the age recipient. Its own format (Q1), so restoring needs no `pg_dump` of a matching version. The
credentials inside stay sealed with the credentials key, so the export alone opens no repository. The maintenance key
writes it (a new storage check, Q10). The gateway never serves `meta/`: it serves `repos/<fqdn>/` only. Each export is
audited; a failure is a problem on the server and an alert.

**2. Off-site.** With `offsite:`, the server writes each export to the off-site bucket's `meta/` too, with the off-site
key, right after the primary one (Q4). Losing the primary bucket and the database together then still leaves an export.

**3. Restoring.** On a rebuilt server: the packages, `server.yaml` and the key files it names, the credentials key put
back, an empty database, then `holdfast-server restore-export --identity <age key file> <export>`. It refuses a database
that is not empty, migrates to the export's schema, loads every table of both schemas, migrates on to the running
version, and audits that it was restored, from which export. Then the server starts as usual: agents enrolled before the
export reconnect with their certificates, whose pins are back; the catalogue is refreshed from the repositories; what
was lost is shown (Q6).

**4. `holdfast host import <fqdn> --escrow-file <decrypted>`**: for a host the export does not have, or with no export
at all. It checks that the file is that host's, proves the password opens the repository with the maintenance key
(`restic cat config`), and adds the host with that repository and password, and an enrollment code. No S3 key: hosts
have none. No job: the admin sets them again, from the config in the export if there is one.

**5. The runbook**, `docs/runbooks/rebuild-holdfast.md`: what to have offline (the age key, the credentials key, the
packages, `server.yaml` and its key files, or new keys made in the storage's console), the steps in order, how to check
each one, and what is lost (Q6).

## 3. Tests

- **e2e:** a nightly export written to the bucket, and to the off-site bucket when `offsite:` is set; a fresh database
  restored from it, and the server started on it: agents reconnect with their certificates, the config, history and
  audit log are back, off-site's too, the catalogue refreshes; a refused restore into a database that is not empty; an
  agent newer than the export (Q6); `host import` from an escrow file and its password check.
- **Lab:** the runbook walked on hfsrv: its database dropped and `/etc/holdfast` and `/var/lib/holdfast-server` removed,
  the packages installed again, the export restored, the three agents back without new codes (Q8).

## 4. Order

The export first, then restoring it, then `host import`, then the off-site copy of the export, then the runbook and the
lab.

## 5. Questions

**Q1. The export's format.** Decision 040 says "a PostgreSQL dump".
- *Proposal:* Holdfast's own: each table as `COPY … TO STDOUT` CSV in a tar, with the schema version, restored by
  `holdfast-server restore-export`. It needs no PostgreSQL client tools on the server, works whatever PostgreSQL version
  the rebuilt server has, and loads into an older schema before migrating forward.
- *The alternative:* `pg_dump --format=custom`, restored with `pg_restore`. Standard, but it needs the client tools at a
  version no older than the server's, and a dump from a newer PostgreSQL may not load into an older one.

**Q2. What is in it.**
- *Proposal:* every table of both schemas except the sessions (they would be stale, and a rebuilt server's users log in
  again) and the gateway's grants (each one lasts one restore, which a rebuild ends). The snapshot catalogue is in it:
  it is a cache, but its tombstones (what Holdfast deleted, and when; what was deleted on request; what was
  acknowledged) cannot be read again from a repository, and without them the "disappeared" and "Deleted in primary"
  views would be wrong after a rebuild.
- *The alternative:* leave the catalogue out, and read it all again; tombstones are lost.

**Q3. When, and how many are kept.**
- *Proposal:* once a day, at 00:30 in the server's default zone, and at start when the last one is older than a day;
  `holdfast export now` for an admin (API, CLI, UI). The server keeps the newest 30 and deletes older ones with the
  maintenance key, in each bucket; bucket versioning then keeps them for the lifecycle rule's days.
  `holdfast_export_last_success_timestamp_seconds` and `HoldfastExportStale` after 2 days. Without an age recipient no
  export is written, and the server warns, as for escrow.
- *The alternative:* the admin's lifecycle rule expires `meta/` (an expiration rule, besides the noncurrent one), and
  Holdfast deletes nothing.

**Q4. The off-site copy of the export.**
- *Proposal:* with `offsite:`, the server writes each export to the off-site bucket under `meta/` with the off-site key,
  right after the primary one, and keeps them there as it keeps them in primary. A failed off-site write is a problem
  and an alert of its own; the primary export still counts.
- *The alternative:* the primary bucket only, and the admin replicates `meta/` with the storage's own tools if they want
  it elsewhere.

**Q5. Restoring.**
- *Proposal:* `holdfast-server restore-export` (on the server, as root; not an API: there is no server yet), into an empty
  database only. One export restores both schemas, primary's and off-site's.
- *Open detail:* the age identity on a file given on the command line, read once and never kept. *Proposal:* yes.

**Q6. What a rebuilt server lacks, and agents ahead of it.** Revisions, history and audit entries after the export are
lost. An agent may run a revision newer than the restored head.
- *Proposal:* the server sends no config to an agent that reports a revision newer than its head, and shows the problem
  "runs revision N, newer than this server's head M: make the change again"; the agent keeps running its own config
  meanwhile. The first save that touches the host sends it as usual. Agents enrolled after the export are refused, as
  their certificate's pin is not in it. They cannot back up meanwhile, since their backups go through the gateway, and
  are brought back with `holdfast host import` (or `host enroll-code` if the export has the host).
- *The alternative:* send the restored config to every agent at once, which could quietly undo a change made after the
  export, a job added included.

**Q7. `host import` in the UI.** UI = API = CLI.
- *Proposal:* all three: `POST /api/v1/hosts/import`, admin everywhere, with the escrow file's contents as input (never
  shown back), and a form in the UI beside Add host.
- *The alternative:* CLI and API only, listed in spec §3.3 as an exception, since a repository password then goes
  through a browser.

**Q8. The lab's walk.**
- *Proposal:* on hfsrv itself: its database dropped, `/etc/holdfast` and the server's state removed, the packages
  removed and installed again, the credentials key, `server.yaml` and its key files put back from the lab's state, the
  export restored; then the three agents back by themselves. It needs no new VM.
- *The alternative:* a new server VM, which the lab would have to make (vms.yml, a sixth VM).

**Q9.** Gone with decision 041: there is no vault program to rebuild. The off-site instance's tables are in the same
database, so the same export holds them (Q2, Q5).

**Q10. The storage contract.** *Proposal:* a new check, B016: the maintenance key may write, read and delete under
`meta/`, and, with `offsite:`, so may the off-site key in the off-site bucket; the gateway key may not read `meta/`
(B007 already tests another prefix: it gains `meta/`).
