# 052: Holdfast runs no commands on hosts

Status: accepted, 2026-11-23. Supersedes [decision 037](037-dump-commands-defined-on-the-host.md). Built in 0.6.0.
**Amended by [decision 055](055-postgresql-databases.md) (2026-11-24):** besides restic, the agent runs PostgreSQL's
own programs (pg_dump, pg_dumpall, pg_restore, psql) for PostgreSQL jobs and their restores, with arguments Holdfast
builds. It still runs no command a person defines, and no hook. Other databases are still backed up from an export the
host writes.

## Decision

- **Holdfast runs no command on a host.** The agent runs restic for its jobs and restores, and nothing before or
  after it.
- **Dump jobs are removed.** Until 0.5.0 a dump job backed up what a command printed: root defined the command in the
  host's `agent.yaml` under `dumps:`, a job named it (`holdfast job set … --dump <name>`), and the agent ran it through
  restic's `--stdin-from-command`. Gone with them:
  - `dumps:` in `agent.yaml`, the `--dump` flag of `holdfast job set`, the UI's "A dump the host defines", and the
    `dump` field of a job in the API, which validation now refuses, citing this decision;
  - the checks D001, D002 and I002, whose codes are retired and never used again;
  - the dump names agents reported.
- **No pre- or post-backup hooks either.**
- **What replaces them is the host's own.** A database, a directory server or anything else that needs an export is
  backed up from a file the host writes itself:
  - a systemd service and timer on the host make the export, with a margin before the Holdfast job's time;
  - the export writes to a temporary name, and is renamed only when it succeeded, so a running or failed export never
    replaces the last good file;
  - a paths job backs up the export's directory;
  - the host's own monitoring watches the unit.

  Holdfast does not order the job after the export, and does not check the file's age.
- **What 0.5.0 left behind:**
  - an `agent.yaml` that still has `dumps:` loads; the agent logs "dumps is ignored", so the host's other jobs keep
    running;
  - a dump job in the config is refused on the next save, with a message that says to give it paths. Giving it paths
    (`holdfast job set <fqdn> <job> --path …`) makes it a paths job. A 0.6.0 agent fails the job's runs with the same
    reason, so it shows as a problem;
  - snapshots made by dump jobs can still be restored, into a new directory only, as before.

## Why

- **We do not want Holdfast to run commands as root on hosts.** That is the reason, and the only one. Decision 037 kept
  the command in the hands of the host's root, but the agent, which runs as root, still started it.
- **Pre-backup hooks were considered and rejected.** A command before a backup needs its own lead time, timeouts and
  exit-code rules. That is a scheduler's job, which systemd already does on every host Holdfast supports, and the
  team's monitoring already watches systemd units.
- **The export and the backup stay apart on purpose.** The host's systemd runs the export and its monitoring watches
  it; Holdfast backs up files. So Holdfast neither waits for the export nor judges the file.

## Consequences

- If an export fails, the job backs up the previous good file again. That costs nothing, as restic deduplicates it,
  and the failed unit is what alerts. A host's monitoring must watch its export units, or the unit's `OnFailure=` must
  send an alert.
- An export that runs past the job's time is not waited for: the job backs up the last good file. The margin before the
  job, and the unit's `TimeoutStartSec=`, are the admin's to choose.
- The export is an ordinary file in a paths job: it shows in Files and Changes, and restores like any other file.
- Database files themselves are still not backed up as files ([decision 003](003-no-database-file-backups.md)): the
  export is.
- The documentation's *Databases and other exports* gives an example, for PostgreSQL. The job form in the UI says that
  a database or a directory server is backed up from the export it writes.
- The restore of a dump snapshot keeps its rules for the snapshots made before 0.6.0: a new directory only, and no
  SELinux label restored for its file.
- The Ansible collection drops `holdfast_dumps`, refuses it when set, and takes out of `agent.yaml` a `dumps:` that
  0.5.0 left there.
