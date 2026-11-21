# 022: Retention cuts are detected by what can still be restored

Status: superseded in part by [decision 030](030-simplify-after-the-architecture-review.md), 2026-11-21.
restic decides what each policy keeps (`forget --dry-run`); Holdfast keeps the union over 7 days, and a cut
is a smaller keep set. The rest of this record stands.

## Decision

A change is a retention cut (R104, which leads to a pending prune) if it reduces **what can still be
restored**. The names of the tiers do not matter. Removing a tier is not a cut if a finer tier covers its
window.

## Why

**Comparing tiers by name gives the wrong answer both ways.**
- **False alarms.** Replacing "one per week for 8 weeks" with "one per day for 60 days" removes a tier but
  keeps more.
- **Missed cuts.** Shortening a window without renaming anything is a real cut.

**Coverage is what the operator cares about.** It is the set of restore points that survive.

*The reasons were inferred when the record was written. You accepted them on 2026-11-21, leaving the call to me ("Whatever you recommend").*

## Consequences

- Cut detection compares the restore points each policy would keep, using the same model as restic. It does
  not compare the config fields.
