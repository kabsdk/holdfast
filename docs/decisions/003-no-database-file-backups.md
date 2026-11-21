# 003: Database files on hosts are not backed up

Status: accepted, 2026-11-21

## Decision

Holdfast does not back up database files on hosts. For example, `/var/lib/pgsql` is excluded.

## Why

A file copy of a running database is not a backup. It can be inconsistent, and a restore can fail silently
or corrupt the database. Calling it protected would be worse than leaving it unprotected.

Databases will be protected by an engine that understands them (decision 001). That is planned for later.

## Consequences

- Whether this exclusion is enforced, and how, is a question in [plan-m1.md](../plan-m1.md) (Q13).
