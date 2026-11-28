# 060: A check that did not pass is retried, on the same part of the data

Status: accepted, 2026-11-28. Amends [decision 030](030-simplify-after-the-architecture-review.md) (the agent's check) and
[decision 059](059-stale-locks-are-removed.md) (`HoldfastCheckFailed`). Built in 0.1-dev. Issue #29.

## Decision

- **A check that did not pass is retried after the first backup that succeeds at least an hour later**
  (`check_retry_after` in `agent.yaml`), where the daily check runs: the server answered, the agent is not paused, no
  backup waits behind it, and the server's daily run, in the middle of the host's quiet stretch, is clear of it.
  - A lock that is still held: up to 3 retries, 1, 2 and 4 hours apart. Each reads no data until the lock is gone.
  - An error, such as damage or the network, or a check stopped with the agent: one retry.
  - A check that ran out of time, or whose restic was killed: none. A retry would cost as much again.
  - A retry does not move the daily check, which stays 23 hours after the last daily one.
- **Every check reads the part of the data that has not passed**, and only a pass moves on to the next part, in
  order. A damaged part fails again on every check until it is repaired, so its alert stays up. While a part stays
  bad, no other part is read. A part is dropped when `check_cycle_days` changes under it (restic refuses `25/10`), and
  the date's part of the new cycle is read.
- **A check on request reads that part too**, a pass clears the retry, and it does not move the daily check.
- **The state is the agent's `check.json`**: when the daily check last started, the part that has not passed, why the
  newest check did not pass, and when its retry is due. It is written before each check starts, so an agent stopped
  during a check neither checks again at once when it starts nor forgets the retries left. An older agent's file,
  with `last` only, reads as nothing pending; a lost file means a check after the next backup.
- **A check's report says why it did not pass and what comes next**: `reason` (`locked`, `damaged`, `error`,
  `timeout`, `killed`), `next_check` (the backup the next check waits for) and `next_is_retry`; a retry says `retry`
  and `retry_of`. The agent's recent runs say it in a few words, "locked by another restic · retry after the 13:30
  backup", with restic's own output in the tooltip and in `--json`, and a retry under By. `holdfast agent events`
  says the same, with a BY column.
- **`HoldfastCheckFailed` says what is true until a check passes.** `holdfast_check_last_run_failed` reads the
  newest check that ended, passed or failed: one stopped with the agent (`interrupted`) neither raises nor clears
  it. Its reason stays `error` while any check since the last pass failed for another reason than a lock, so a retry
  that meets a lock does not turn a possible damage into a lock.
- **Off-site copies are not changed here.** The server's daily check of a copy reads the date's part and is not
  retried; a later change may give it the same.

## Why

- **An alert that outlives its cause costs attention.** In the lab on 2026-11-27, two checks failed on a stale lock
  before decision 059 was installed. The server's prune removed the lock at 03:30 the next night, and backups every
  6 hours went on, but both alerts stayed up, and were sent again every 3 hours, until the next daily check about 24
  hours after the failure: the agent counted its 23 hours from the start of the last check, failed or not.
- **A retry must not clear damage.** The part a check reads was the date's. A check after a failed one read another
  part, passed, and cleared `HoldfastCheckFailed`, and the failed part was not read again for a whole cycle. A retry
  on the date's part would have cleared it within hours. Reading the part that failed until it passes keeps damage
  alerting, and fixes the same gap in the daily check.
- **After the next backup, not on a timer of its own.** Two reviews found that a retry on the agent's own clock
  needs guards the daily check already has: it may meet the server while it restarts and report damage, run into
  the server's daily run, hold up the next slot's backups, and, by moving the daily anchor, skip a day's check on
  hosts with daily jobs. After the next backup, an agent with jobs every 6 hours clears within 6 hours; one with daily
  jobs gains nothing, and its next backup is its next daily check anyway.
- **Back-off for a lock.** One hour is past restic's 30 minutes, after which a lock left by a killed restic is stale
  and removed (decision 059). A lock that is still held is a long prune or another restic: 3 tries, then the daily
  check, so a host's loop is not tied up waiting for it.
- **An interrupted check says nothing of the repository.** Counting it as not failed cleared a failure that nothing
  had checked again.
