# 023: Restore defaults: everything selected, into a new root-only directory

Status: accepted, 2026-11-21

## Decision

**Selection.**
- Everything is ticked by default. This is an exclusion model: anything unticked appears as a **Left out**
  chip.

**Target.**
- The default target is a new directory that only root can read:
  `/var/restore/<date>-<user>-<snapshot>`, mode 0700. Restored files keep their owner, mode and ACLs.
- An in-place restore first backs up what it will replace.

**Running it.**
- A dry run is available.
- The restic command uses whichever of `--include` or `--exclude` gives the shorter list, plus
  `--sparse --verify`.

## Why

- **Nothing is left out by accident.** Under the exclusion model, what is left out is visible.
- **The safe default never overwrites.** A new root-only directory cannot overwrite anything or expose
  files to other users, and it keeps the metadata exactly.
- **An in-place restore can be undone**, because of the backup taken first.

*The reasons are inferred. Confirm or correct them.*

## Consequences

- An in-place restore needs `restore.in_place` (admin only).
