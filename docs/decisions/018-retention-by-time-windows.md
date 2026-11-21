# 018: Retention by time windows, never by counts

Status: accepted, 2026-11-21

## Decision

Retention is expressed only as time windows:
- `--keep-within X`
- `--keep-within-hourly X`, `--keep-within-daily X`, `--keep-within-weekly X`, `--keep-within-monthly X`
  and `--keep-within-yearly X`
- "always keep the newest N" (`--keep-last N`, default 3)

Count-based flags (`--keep-hourly N`, `--keep-daily N`, and so on) are never used.

## Why

A count means a different length of time on every schedule. It has been verified that `--keep-daily 14` on
a weekly schedule keeps 14 **weeks**, not 14 days. A time window means the same thing whatever the schedule.

`--keep-last` is the only count, and it acts only as a floor. It ensures that the newest snapshots survive
whatever the windows say.

It has also been verified that restic keeps the oldest snapshot when a tier cannot be filled.

## Consequences

- The PoC's retention model test uses count flags. It has to be rewritten for time windows (M3).
- The PoC verified "keeps the oldest snapshot" only with a count flag (`--keep-yearly 2`). It must be
  re-verified for the `--keep-within-*` flags before Holdfast relies on it (plan-m1 L1).
