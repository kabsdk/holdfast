# 033: What each role sees

Status: accepted, 2026-11-21. You left it to me ("You decide, continue"). It refines decision 025 and spec
§6.2, and lands in M4 with the roles.

## Decision

- **No binding for a host means the host does not exist for you.** It is not listed, not searchable and not
  counted in the overview. Prometheus and Grafana sit outside Holdfast, so whoever can see them still sees
  host names there.
- **viewer** sees that backups exist and run: hosts, jobs, their schedule and retention, status, history
  and alerts. It sees no file names. Files, Changes, Find and restore targets are not for viewers.
- **editor** also browses names and metadata (Files, Changes, Find), because a restore needs them. It
  restores into new directories and changes jobs.
- **admin** does the destructive things: in-place restores, retention cuts, Prune now, Delete now,
  decommission.
- **Nobody sees file contents** through Holdfast (decision 030). Changes is restic's own diff, at file level
  (spec §5.3).

## Why

- **File names can be sensitive on their own,** for example `/home/anna/medical/…`. With this split you can
  give someone "are the backups OK?" without the folder tree.
- **Three roles and no inheritance stay,** as decision 025 says. Only what `view` covers changes: it is split
  into `view` (existence and status) and `browse` (names and metadata).
