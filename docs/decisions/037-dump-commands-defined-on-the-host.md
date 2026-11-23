# 037: Dump commands are defined on the host

Status: accepted, 2026-11-22.

## Decision

- A dump job backs up what a command prints, with `restic backup --stdin-from-command`, as one file in the snapshot.
- **The command is defined in the host's `agent.yaml`, under a name.** A job only names it (`dump: postgresql`).
  The server never sends a command.
- The agent runs it as the user `agent.yaml` names (default root), with a clean environment: `PATH`, `LANG`, and
  the variables the dump lists. It never sees restic's environment, which holds the host's S3 key.
- The agent reports its dumps' names, not their commands, so the server can check a job (D001), and refuses a dump
  job for an agent that does not know dumps (D002).
- A dump is restored into a new directory, never in place. Loading it is the admin's job, on the host.

## Why

- **Otherwise anyone who may change jobs could run any command as root on every host.** With the command on the
  host, a job editor can only choose among what the host's root has allowed.
- **restic decides whether the dump worked:** a command that exits non-zero, or is not there, fails the backup,
  and no snapshot is made (checked against restic 0.19.1). So a half-written dump is never kept as a whole one.
- **The clean environment matters:** restic passes its own environment to the command, and a dump often runs as a
  less privileged user, such as `postgres`.
