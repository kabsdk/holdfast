# 013: No rollback

Status: accepted, 2026-11-21

## Decision

There is deliberately no rollback to an earlier revision. To undo a change, edit the config and save it.
That save passes the same checks as any other change.

## Why

A rollback is a bulk edit in disguise. Rolling back to revision N reverts every change made since N, on
every host and job. That can silently cut retention, and it would skip the checks that a hand-made edit
would go through, such as R104 and the 7-day protection.

## Rejected alternative

A rollback that runs the full checks. Even with checks, a rollback asks the operator to reason about many
changes at once. An edit names exactly what changes.

## Consequences

- There is no "restore revision N" in the UI, the API or the CLI.
- Any two revisions can be diffed, so the operator can see what to edit back.
- **Undo changes** in an editor only discards unsaved edits. It is not a rollback.
