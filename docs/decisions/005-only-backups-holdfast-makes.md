# 005: Only backups that Holdfast makes itself

Status: accepted, 2026-11-21

## Decision

The console shows only backups that Holdfast makes itself. There is no "watched only" mode for backups
made by other tools.

## Why

Holdfast can only vouch for what it configures, schedules, checks and can restore. A watched-only backup
would show a green status for something whose retention, restorability and credentials Holdfast does not
control. That looks like protection without being protection.

*The reason is inferred. Confirm or correct it.*

## Consequences

- No importers for foreign repositories or for other backup tools' status.
- What to do with snapshots that appear in a Holdfast repository without a job tag is an open point
  (plan-m1 Q12).
