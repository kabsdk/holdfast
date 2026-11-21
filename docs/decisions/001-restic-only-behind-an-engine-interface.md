# 001: restic is the only engine, behind an engine interface

Status: accepted, 2026-11-21. The engine interface is deferred until a second engine exists
([decision 030](030-simplify-after-the-architecture-review.md)). restic only stands.

## Decision

Holdfast configures, schedules, monitors and restores **restic** backups, and nothing else.

A job is an engine behind one interface. Each engine declares its capabilities: identity, schedule/run,
freshness, restore points, browse/changes, restore, and retention/prune. The UI, API and CLI show only what
the job's engine supports.

A PostgreSQL-native engine (physical base backups, WAL archiving, PITR) is planned for later.

## Why

- Holdfast never re-implements a storage format. Every byte goes through restic, so a repository can still
  be restored with the plain `restic` binary if Holdfast is gone. (PoC README.)
- A single engine keeps the product small enough to test end to end.
- The interface exists now because the planned PostgreSQL engine has different capabilities. For example,
  its restore points are a continuous range, not a list of snapshots. The UI must not offer what an engine
  cannot do.

*The reason for "restic only" is inferred from the PoC. Confirm or correct it.*

## Review

2026-11-21: re-examined in [research/engine-review.md](../research/engine-review.md). No change is proposed.
restic is the only candidate that passes every gate. rustic (once production-ready) and Kopia (if it gains
ACL and xattr support) are worth re-checking.

## Consequences

- No other backup tool is integrated or wrapped.
- Code that serves the UI, API or CLI asks the engine for its capabilities. It does not assume restic.
