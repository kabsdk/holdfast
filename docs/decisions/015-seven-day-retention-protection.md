# 015: The 7-day retention high-water mark, with Prune now as the only bypass

Status: accepted, 2026-11-21

## Decision

**High-water mark.** Maintenance never prunes a snapshot that any policy in force during the last 7 days
would keep. This holds whatever the config says.

**Pending prune.** A retention cut becomes a pending prune for 7 days, however the cut was made: an edit, a
library update, the API or the CLI. During those 7 days a one-click **Keep \<longer\> retention** action is
offered.

**Prune now** is the only bypass:
- Only an admin can use it (`retention.cut`).
- It has a confirmation page showing the counts, the date range, the full paginated list of snapshots and
  the exact restic command.
- The admin confirms by typing the host's short name.
- It is audited, and the deleted IDs stay findable in search.

## Why

A retention mistake, or a compromised editor account, must not be able to destroy history instantly.
Seven days gives people time to notice a pending prune and keep the longer retention.

*The reasons were inferred when the record was written. You accepted them on 2026-11-21, leaving the call to me ("Whatever you recommend").*

## Consequences

- Maintenance must remember every policy that was in force during the last 7 days, per job.
- No milestone may add a delete path (forget, prune or decommission) before this protection exists in M3.
  The exceptions are Prune now and decommission, which have their own confirmations.
