# 058: A rollback picks what to bring back, and the gateway holds the agent's writes

Status: accepted, 2026-11-26. Amends [decision 038](038-rolling-a-repository-back.md). Built in 0.1-dev.

## Decision

- **What a rollback can undo is listed.** Each moment something in the repository was deleted or overwritten, from
  S3's versions, newest first, back to the earliest a rollback reaches: what went (snapshots and their jobs, and the
  other files by kind) and who did it, one of Holdfast's runs (the daily retention, Run retention now, Prune now,
  Delete now, a rollback) or not Holdfast. Changes no run of Holdfast's explains are one moment until they are quiet
  for 10 minutes; an overwrite is never Holdfast's, as restic never overwrites a file. `GET
  /api/v1/agents/{agent}/rollback` without `to`, `holdfast agent rollback <agent>` without `--to`, and the rollback
  page.
- **Bringing one back rolls back to just before it.** The preview and the confirmation are decision 038's. The page
  has no date or time field; the API and the CLI still take any time.
- **The gateway holds the agent's writes while a rollback runs.** The agent's own repository is read-only for it until
  the rollback is done, online or not: its writes get 503 and restic retries, or the backup fails and is retried
  later. An online agent is still paused, so it does not log failed backups. `agent_down`, `--agent-down` and the
  page's "The agent is down" go: an offline agent, or one too old to pause, no longer stops a rollback.

## Why

- **A time was a guess.** The repository changes only when something is written or deleted, so every time between two
  changes gives the same result. The moments worth rolling back to are just before something went, and the server
  knows them from S3's versions; it showed them only after a time was picked.
- **Who did it matters.** A deletion by the daily retention is expected, one behind Holdfast's back is not. Bringing
  back an older moment also undoes the newer ones and sets aside the backups made since; the list and the preview say
  so.
- **An admin should not have to vouch that a host is down.** Agents hold no S3 key and write only through the gateway
  (decision 041), so the server itself makes sure nothing writes into the repository while it rolls it back, whatever
  the agent's state.
