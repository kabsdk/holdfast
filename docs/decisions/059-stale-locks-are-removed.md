# 059: Holdfast removes stale restic locks itself

Status: accepted, 2026-11-27. Amends [decision 030](030-simplify-after-the-architecture-review.md) (the agent's check) and
[decision 055](055-postgresql-databases.md) (a dry run of a dump). Built in 0.1-dev.

## Decision

- **When restic finds the repository locked** (exit 11), Holdfast runs `restic unlock`, and runs the command once more
  if that removed a lock: the agent's backups, checks and restores, and the server's forget, check and prune. A live
  lock stays, and the command fails as before.
- **After a restic killed by a signal** (SIGKILL, the OOM killer, a broken pipe), the agent runs `restic unlock` at
  once: that restic had no time to remove its own lock.
- **The server's run removes stale locks before its first step**, the daily run as Prune now and Delete now, so its
  forget does not wait 10 minutes (`--retry-lock`) for a lock that will never go. It leaves a host's locks alone while
  the host's clock is more than 5 minutes off its own, and audits each removal as `repository.stale_locks_removed`.
  The agent logs its own.
- **Only stale locks, never `--remove-all`**, as before. restic decides which are stale: one not renewed for 30
  minutes, while a running restic renews its own every 5, or one made on the same machine by a process that is gone.
- **A dump is read to its end.** A dry run reads the whole dump: `pg_restore --list` reads only its table of contents,
  and the agent reads the rest and drops it, so restic finishes and removes its lock, and a dump that cannot be read to
  its end fails the dry run. After a `pg_restore` or `psql` that failed, restic is stopped with SIGINT, which removes
  its lock. `psql` commits only once the whole dump was read.
- **`HoldfastCheckFailed`**, a ticket: an agent's newest repository check failed, with `reason` `locked` or `error`,
  from `holdfast_check_last_run_failed`. `HoldfastMaintenanceStale` still fires after 3 days without a check that passed.

## Why

- **restic never removes a stale lock by itself.** A command that meets one fails every time, until someone runs
  `restic unlock` with the repository's password. Found in the lab on 2026-11-24: a dry-run restore of a PostgreSQL
  dump closed restic's output once `pg_restore --list` had the table of contents, restic died of SIGPIPE at its next
  write, and its lock stayed. Every repository check and prune of that host failed for three days. Backups went on,
  as a backup's lock is shared, so nothing said so until `HoldfastRetentionFailing`, and nothing in Holdfast could
  clear it: its error said to run `unlock`, which Holdfast has no command for.
- **It is restic's own command, used as restic means it** (decision 030: no protection that restic itself lacks).
  Removing stale locks is what `restic unlock` is for, and what restic tells the operator to do.
- **The clock.** restic judges a lock by the time in it, which the machine that made it wrote. A host whose clock is
  far behind the server's makes locks that look old to the server while their restic still runs. The agent removes
  locks with its own clock; the server only while the host's clock is close to its own. `HoldfastAgentClockSkew`
  fires at a minute.
- **The pipe stays.** A pipe has no size limit; the lock was left because restic was cut off, not because the dump was
  large. Writing the dump to a file first would need a disk as large as the largest dump, leave a decrypted copy of
  the database on it, and make pg_restore wait for the whole download. Reading the dump to its end also proves all of
  it can be read, which is what a dry run is for.
- **A failed check is said at once.** A check that fails may mean damage in the repository; 3 days of
  `HoldfastMaintenanceStale` was too long to wait for that.
