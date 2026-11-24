# 055: PostgreSQL databases, dumped with pg_dump into restic

Status: accepted, 2026-11-24. Narrows [decision 002](002-kubernetes-out-of-scope.md), fulfils
[decision 003](003-no-database-file-backups.md), bends [decision 004](004-fqdn-identity-one-repository-per-host.md)
and amends [decision 052](052-holdfast-runs-no-commands-on-hosts.md). Built in 0.1-dev.

## Decision

- **A job of the engine `postgresql` backs up one PostgreSQL database, or a server's roles and other globals.** The
  agent runs `pg_dump --format=custom --compress=0` of the database, or `pg_dumpall --globals-only
  --no-role-passwords`, through restic's `--stdin-from-command`. The dump goes into the agent's repository as one
  file, `<database>.dump` or `globals.sql`, in a snapshot tagged `job=<id>`. restic still stores, encrypts,
  deduplicates, keeps and forgets every byte, behind the same 7-day protection. A pg_dump that fails makes no
  snapshot, and the run fails.
- **One job a database.** Each has its own schedule, retention, calendar and restores. `holdfast job set <agent>
  --connection <name> --database app --database wiki --globals` makes a job each, named after its database, and
  `roles` for the globals, in one revision.
- **Connections are set centrally**, in the UI, the API or the CLI, on the agent that reaches the database server: a
  name, libpq's URL without a user, a password or a database, the backup user and its password, and optionally a
  restore user and its password. The URL and the users are part of every revision. The passwords are not: they are
  sealed with the credentials key, as repository passwords are, sent only to that agent, never shown, and in the
  nightly export still sealed. Setting or removing a connection is `config.storage`, an admin's; testing one is
  `config.jobs`.
- **Saving a connection makes the agent test it.** It connects as the backup user, as a backup would, and reports the
  server's version, TLS, its own pg_dump, and each database with its size and owner. With a restore user, it reports
  whether that user may create databases, and whose roles it has. The job form offers those databases.
- **What only root on the agent sets stays in its `agent.yaml`**: `postgresql.bin_dir`, the directory with pg_dump,
  pg_dumpall, pg_restore and psql, and `postgresql.allow_in_place_restore`, off unless root turns it on. Without
  `bin_dir` the agent takes no PostgreSQL job.
- **A dump restores onto its own database server only**, in one of three ways:
  - into a new database beside the original, named `<database>-restore-<YYYYMMDD>-<HHMM>` from the dump's time and
    owned by the original's owner. It writes nothing that exists, so it needs `restore.new_dir`, as a new folder does;
  - in place, over the database it came from: an admin's (`restore.in_place`), who types the database's name, and only
    where the agent's `agent.yaml` allows it. The agent first dumps the database as it is, into a snapshot tagged
    `pre-restore`, then runs `pg_restore --clean --if-exists --single-transaction --exit-on-error`: if anything
    fails, nothing changed. Tables made after the dump stay. A database that is gone is made again;
  - as the dump file, into a new folder, as any file is restored. Roles and globals restore this way only: Holdfast
    never creates roles.

  Never into another database by name, never onto another agent, and never in place from an off-site copy.
- **Checks** (spec §4.5): P001, the agent's pg_dump is older than the server (error); P003, the database was not on
  the server at the connection's last test (warning); P004, the agent is too old for PostgreSQL jobs, or has no
  PostgreSQL client (error); P005, the connection was never tested, or its last test failed (warning). The problem
  `postgresql` is a connection whose last test failed, and P001 is a problem on the agent too.
- **A container image**, `ghcr.io/kabsdk/holdfast-agent`, holds the agent, the pinned restic and PostgreSQL 18's
  client, to back up a database server in Kubernetes from a pod: one Deployment per database server, its agent named
  after it ([decision 056](056-agents-named-in-their-own-config.md)). It needs no access to the Kubernetes API.

## Why

- **pg_dump is PostgreSQL's own consistent backup.** It reads the database in one transaction, so its dump is a
  backup, which a copy of the files of a running database is not (decision 003). Uncompressed custom format lets
  restic compress and deduplicate it, and pg_restore read it.
- **restic already does the rest**: encryption on the agent, deduplication, retention with its protection, the
  gateway, off-site copies and the storage witness. A dump is one more file for it.
- **These are Holdfast's own programs, not people's commands.** Decision 052 stopped Holdfast from running commands
  people define, as root, before a backup. That still holds: no hooks, no dump commands in `agent.yaml`. The agent
  runs restic, as it always did, and PostgreSQL's programs from the directory root gives it, with arguments Holdfast
  builds, in a clean environment, the password in a file and never on a command line.
- **Connections are central so that one place shows what is backed up.** The brief had them in `agent.yaml`. On the
  server they have revisions, the same UI, API and CLI, and a test whose list of databases the job form uses. The
  password must reach the agent anyway; sealing it as the repository passwords are keeps one way of holding secrets.
  Root on the agent still decides what the agent can do: which client, and whether a restore may replace a database.
- **A database in Kubernetes is a database.** Decision 002 keeps Kubernetes volumes and API objects out, and that
  stands. A database server reached over the network is backed up like any other, wherever it runs. CloudNativePG's
  own backups can stay beside it.
- **A restore never names another database**, so a typo cannot overwrite the wrong one. Replacing a database needs an
  admin, root on the agent, and the database's name typed.

## What it costs

- Each dump is a whole copy: no point-in-time recovery, and a large database takes a backup's time to read in full.
  The physical engine, with base backups and WAL archiving, stays planned (spec §1.3).
- The backup user reads every database (`pg_read_all_data`, and `BYPASSRLS` where tables have row security). Whoever
  holds its password, or is root on the agent, can read them all.
- pg_dump must be at least as new as the server: upgrading a database server means upgrading the agent's client
  first (P001).
- An agent in Kubernetes stands for a database server, not a machine: its repository is that server's.
