# 007: The server is the maintenance leader, and agents never forget or prune

Status: accepted, 2026-11-21

## Decision

- Only the server runs forget, prune and check.
- With high availability, only the server that holds the VIP runs maintenance. restic's exclusive lock is a
  second guard against split brain.
- Agents never forget and never prune.

## Why

- **A compromised host cannot destroy its own history.** The agent's S3 key may delete only under `locks/`
  (decision 011), so the agent cannot remove snapshots or data, even as root on the host.
- **The retention rules are enforced in one place.** The 7-day protection and Prune now (decision 015) need
  a single place that sees every policy in force and every change to it.

*The reasons were inferred when the record was written. You accepted them on 2026-11-21, leaving the call to me ("Whatever you recommend").*

## Consequences

- The server holds every host's repository password. That is why the escrow bundle and the protection of
  credentials at rest matter.
- Maintenance takes restic's exclusive lock, and while it holds the lock, backups cannot take theirs.
  How the two coexist is a question (plan-m1 Q7).
