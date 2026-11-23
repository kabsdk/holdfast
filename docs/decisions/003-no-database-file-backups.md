# 003: Database files on hosts are not backed up

Status: accepted, 2026-11-21

## Decision

Holdfast does not back up database files on hosts. For example, `/var/lib/pgsql` is excluded.

## Why

A file copy of a running database is not a backup. It can be inconsistent, and a restore can fail silently
or corrupt the database. Calling it protected would be worse than leaving it unprotected.

Databases will be protected by an engine that understands them (decision 001). That is planned for later.

## Consequences

- No built-in exclude enforces it: the agent always leaves out only the restic cache and its own state. A database is
  protected by backing up an export the host writes itself: a systemd timer on the host runs the database's own export
  command, such as `pg_dumpall`, and a paths job backs up the file
  ([decision 052](052-holdfast-runs-no-commands-on-hosts.md)). Until 0.6.0 a dump job did this
  ([decision 037](037-dump-commands-defined-on-the-host.md)).
