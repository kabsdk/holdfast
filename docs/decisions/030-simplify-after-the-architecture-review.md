# 030: Simplify after the architecture review

Status: accepted, 2026-11-21. You accepted the review's recommendations, with two exceptions noted below.

## The principle

**Holdfast is a restic manager with sane defaults and good features. It is not a fix for everything that
can go wrong with restic.** It does not protect against what restic itself cannot. A host's clock jumping
is one example.

## Fixes to what is built (M1)

1. **Agent TLS pinning.**
   - Verify the server's certificate against the pinned CA as the only root, and check the hostname.
   - Today, any certificate in the chain that matches the pin is accepted, so a man in the middle can pass
     the check.
2. **Job status follows the order in which the server receives reports, not the agent's clock.**
   - This is Holdfast's own monitoring, not protection for restic.
   - Today, one report dated in the future freezes the job's status.
3. **The storage witness's alerts do not depend on the server.**
   - The witness exports its own staleness threshold for each host.
   - Today, the rules need series that only the server exports.
4. **The maintenance key is narrowed.**
   - `storage.md` grants only the actions Holdfast needs, not `s3:*`.
   - New storage checks: the maintenance key cannot delete object versions for good, and cannot change
     versioning, the lifecycle rule or the bucket policy.
   - Another check: a lifecycle rule for noncurrent versions exists. How long it keeps versions is still
     Q4's open question.
5. **Enrollment checks the code before it touches storage.**
6. **Each restic run on the agent has a timeout.**
7. **The repository check moves to the agent.**
   - After a backup, inside its one-restic-at-a-time loop, the agent runs `restic check
     --read-data-subset` with the host's key. It can never meet the host's own backups.
   - The server checks a repository only before it prunes it (M3).
   - The server's daily check, and its recovery from an interrupted check, go.
   - The quiet-window placement stays, for prune in M3.

## Changes to the plan

8. **No warm standby.** This replaces decision 010.
   - Agents back up without the server, so an outage costs only reporting and maintenance.
   - Instead: a nightly PostgreSQL dump and config export to site B, the sealing key in escrow, and a
     tested rebuild runbook.
   - M6 loses its failover work.
9. **Retention is decided by restic.** This replaces the model in decision 022.
   - Run `restic forget --dry-run --json` for each policy in force during the last 7 days.
   - Keep the union of what those runs keep, and forget the rest by snapshot ID.
   - A retention cut (R104) means the new keep set is a strict subset of the old one.
10. **Cut or deferred:**
    - **Cut:** the library, profiles and "Jobs with the old values" (M5).
    - **Cut:** `content_view`, which replaces decision 026. No file contents ever leave a host.
    - **Deferred:** the engine abstraction of decision 001, until a second engine exists.
11. **The 21 decision records whose reasons were inferred** go to you, to confirm or delete.
12. **Off-site corrections:**
    - No key is shared across sites, which drops O10's "same key on both clusters".
    - A restore from site B reads the whole repository. Drop the "one snapshot, for a limited time" grant.
    - A stalled site B blocks site A's prune for a bounded time only.
13. **Restores are bounded on the host (M2).**
    - The agent's local config sets them, and only host root can change it.
    - The default is restore into `/var/restore` only.
    - In-place restore only on hosts that opt in.
    - Only snapshots this agent made.
    - So a compromised server or admin login is not root on every host.
14. **The spec says where the real floor is.** Against a compromised server, and against Prune now, the
    protection is S3 versioning plus the lifecycle expiry, not the 7 days.
15. **Added to the plan:**
    - database and directory dumps as a typed job, using `restic backup --stdin-from-command`;
    - restore to another host, and a bare-metal recovery runbook that uses the escrow bundle;
    - rolling a host's prefix back to a point in time from S3 versions;
    - retry within the schedule's gap, plus `Nice=`, `IOSchedulingClass=` and an optional upload limit
      (this answers Q7's "no retry" and Q11);
    - repository size and growth, including noncurrent versions;
    - a host-compromise procedure: new S3 key, new restic key, revoked token, last good snapshot;
    - where site B's own hosts back up to, and what they do when the sites are cut off from each other;
    - packaging and upgrades, with protocol version negotiation between agent and server;
    - alerting without Alertmanager, including what to run for the dead man's switch;
    - at host add, the repository password, encrypted to an offline `age` recipient, written to the bucket
      and to site B (Q5's bundle format).

## Not accepted

- **No guard in retention against snapshots dated in the future,** by the principle above.

## Still open

- **How off-site copies are made:** holdfast-vault with `restic copy` (decision 027), or a mirror that
  copies the repository's files from A to B as they are, and never overwrites or deletes. Either way, site B
  runs a small service of its own for the copies, B's retention and status.

## Why

The review found two security holes in M1, and plans that were too large for several hosts. This is
the complexity you objected to. Each change above either closes a hole, uses restic instead of
re-implementing it, or removes work that this audience does not need.
