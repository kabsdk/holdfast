# 032: Changes made outside Holdfast are shown, and only Holdfast's own files are repaired

Status: accepted, 2026-11-21. You left the details to me ("Do whatever you recommend").

## Decision

**Holdfast owns the restic config.** A host has no restic config to edit. The jobs are on the server, and
the agent runs restic with exact arguments each time.

**Holdfast repairs its own files on a host, and records that it did.**
- The cached host config: if it no longer matches its hash, the agent ignores it and the server sends it
  again. Until then the host shows as drifted. This was already so.
- The credential files: before each run, the agent checks them against what the server sent. If they
  changed, it records a `notice` in the host's history and fetches them again. Built in M1.

**Holdfast never undoes changes made to a repository by hand. It shows them in red, alerts, and offers a
fix.** With the snapshot catalogue (M2):
- **A snapshot that Holdfast did not make.** It is left alone. Holdfast shows how much space it takes.
- **A snapshot that Holdfast knew, gone without Holdfast deleting it.** Holdfast shows until when it can be
  recovered from S3's old versions, and offers to roll the host's prefix back (decision 030).
- **A restic password added to the repository outside Holdfast.** The agent's daily check lists the
  repository's keys.

**What a host's root cannot do by hand** is delete its own backups. The host key may delete only lock
files, so `restic forget` and `prune` fail there with Access Denied (B005, tested). Root on the server
holds every key, so Holdfast cannot stop it. Against that, the protection is in storage: no Holdfast key
can delete old versions. Site A cannot touch site B either.

**restic itself.**
- The agent's package ships the pinned restic at `/usr/libexec/holdfast/restic`, and a new restic comes
  with a new agent package.
- The agent never downloads anything.
- The server shows a warning, and alerts after an hour (`HoldfastResticNotPinned`), when an agent reports
  a version other than the one Holdfast is tested with. Built in M1.

## Why

- **Holdfast's own files are Holdfast's to keep right.** Repairing them is not a judgement about anyone's
  intent.
- **Undoing a change to a repository is.** An admin may have had a reason. Silently fighting them, or
  rebuilding restic's state by hand, is the "fixing restic" that decision 030 rules out. Saying so clearly,
  and offering the fix, is the restic manager's job.
- **A pinned, bundled restic means every host runs what was tested.** The distributions ship different and
  older versions.
