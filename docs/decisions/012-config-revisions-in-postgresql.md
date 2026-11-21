# 012: Config lives as revisions in PostgreSQL, with no Git and no approvers

Status: accepted, 2026-11-21

## Decision

Holdfast is the authority for config.
- Every save is a revision in PostgreSQL: its number, who made it, when, a message, and the full config
  (hosts, jobs and library).
- Any two revisions can be diffed.
- Drift between the config and what an agent runs is detected and shown.
- There is no Git and there are no approval steps.

## Why

- **One source of truth and one path for every change.** All changes, from the UI, the API or the CLI, go
  through the same checks and land in the same history.
- **Git would add a second writer.** That would mean merge conflicts and a second audit trail.
- **Approvers would add little.** Retention cuts are already protected by the 7-day high-water mark, and
  Prune now has its own confirmation.

*The reasons are inferred. The PoC assumed Git ("in production this lives in Git"), so this reverses the
PoC. Confirm or correct the reasons.*

## Consequences

- Revisions are immutable and numbered in a single line.
- Credentials are never part of a revision, so a revision can be shown and diffed without restriction.
