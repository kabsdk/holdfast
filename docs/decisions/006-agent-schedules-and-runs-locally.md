# 006: The agent schedules and runs backups locally

Status: accepted, 2026-11-21

## Decision

- **The agent schedules and runs backups itself.** It runs as root on each host, keeps a cached copy of its
  config, runs restic on its own schedule, and spools events to disk until the server has acknowledged them.
- **The agent always dials out.** It connects to the server by HTTPS long-poll, and each poll is also a
  heartbeat.
- **The server never starts a scheduled backup.**

## Why

Backups must keep running while the control plane is down. The PoC test
`TestAgentKeepsBackingUpWhileServerIsDown` proved this: 7 snapshots were made during a 15-second server
outage, and all 7 events were delivered afterwards.

Because the agent dials out, a lost session never cancels running work, and hosts need no inbound port.

## Consequences

- An outage of the server or PostgreSQL delays reporting but not backups.
- Every run ends in a reported status, even if the agent is killed (`interrupted`). Nothing fails silently.
- The agent runs as root, because reading every file and restoring owners, ACLs and SELinux labels needs
  root.
