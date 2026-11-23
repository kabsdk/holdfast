# 020: Library items are copies, not links

Status: superseded by [decision 030](030-simplify-after-the-architecture-review.md), 2026-11-21: the library
is cut.

## Decision

**Picking an item copies its values.** When a paths item, a protection item or a profile is picked, its
values are copied into the job.
- Editing any of those fields in the job switches the job to **Custom**. It stays Custom even if its
  values later match an item again.
- There is no "based on" link and no "reset to".

**Editing an item never changes jobs by itself.** After a library item is edited, Holdfast lists **Jobs with
the old values (N)** with checkboxes.
- Each job is checked against its own config.
- One click updates all the selected jobs in a single revision.
- Warnings must still be accepted, and retention cuts still go through the 7-day protection.

## Why

- **A linked preset is a hidden bulk edit.** Changing it would silently change the schedule and retention of
  every job that uses it, which is the same danger that rules out rollback (decision 013).
- **Copies make the change visible.** Each job's config says exactly what that job does, and any bulk update
  is explicit, checked per job and recorded in one revision.

## Consequences

- No job stores a reference to a library item that changes its behaviour.
- "Jobs with the old values" is found by comparing values. It does not follow links.
