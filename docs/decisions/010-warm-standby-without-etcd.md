# 010: A warm standby, without etcd

Status: superseded by [decision 030](030-simplify-after-the-architecture-review.md), 2026-11-21: no warm
standby. A nightly dump, the sealing key in escrow, and a tested rebuild runbook take its place.

## Decision

High availability is a warm standby in site B:
- a keepalived VIP;
- a PostgreSQL streaming replica.

There is no etcd and no other consensus store. Only the VIP holder runs maintenance, and restic's exclusive
lock is the second guard. Agents keep backing up during a control-plane outage.

## Why

- **Agents protect themselves.** They schedule and run backups locally (decision 006), so a control-plane
  outage costs only reporting and maintenance. It does not cost backups.
- **A quorum store would be a cost without a benefit.** It would need a third failure domain and would add
  its own failure modes. Here it would buy little.
- **PostgreSQL already holds the state** and already replicates it.

## Consequences

- Split brain is guarded by the VIP and by restic's exclusive lock, not by a quorum.
- The failover test ("kill site A") is M6.
