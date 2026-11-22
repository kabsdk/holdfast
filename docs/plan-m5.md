# Plan for M5: operating Holdfast, and recovering with it

Status: accepted, 2026-11-22. You answered the questions in §5 with "Go", so every proposal stands as
written. By Q1, M5 is items 1–7, and items 8–10 are a new M6 (recovering with Holdfast), with off-site copies
moving to M7 (CLAUDE.md is changed to match). M6 gets its own plan, which carries Q5, Q6 and Q8 over as
answered. Q3 is decision 036.

## 1. What M5 holds

CLAUDE.md says M5 is to be re-planned with what decision 030 adds. Those additions, with decommission,
which M4 left out (plan-m4 Q7):

| # | Feature | From |
| --- | --- | --- |
| 1 | **Packages and upgrades**: `.deb` and `.rpm` for the agent, the server, the CLI and the witness; systemd units; the agent and the server agree on a protocol version | 030 §15 |
| 2 | **Run controls**: a failed backup is retried before the next run is due; the agent's restic runs at low CPU and I/O priority; an optional upload limit | 030 §15 |
| 3 | **Repository size and growth**, including S3's old versions | 030 §15 |
| 4 | **The repository password in escrow at host add**: encrypted to an offline `age` key and written to the bucket | 030 §15, plan-m1 Q5 |
| 5 | **When a host is compromised**: new restic password, revoked agent token, and the runbook | 030 §15 |
| 6 | **Alerting without Alertmanager**, and the dead man's switch: documentation | 030 §15 |
| 7 | **Decommission**: deleting a removed host's repository, after a wait | plan-m4 Q7 |
| 8 | **Dumps**: a database or a directory, piped into restic with `--stdin-from-command` | 030 §15 |
| 9 | **Restore to another host**, and the bare-metal recovery runbook | 030 §15 |
| 10 | **Roll a host's repository back** to a point in time, from S3's old versions | 030 §15, decision 032 |

**Not in M5:** off-site copies and the vault (M7), where site B's own hosts back up to (M7), and automation
tokens (plan-m4 Q4).

## 2. Design in brief

**1. Packages.**
- Built with nfpm from the same binaries, in a container, so the build needs no packaging tools on the
  host.
- The agent's package ships the pinned restic at `/usr/libexec/holdfast/restic` (decision 032). The agent
  never downloads anything.
- The agent's unit runs with `Nice=10` and `IOSchedulingClass=idle`, so a backup gives way to the host's
  own work (item 2).
- The agent sends its protocol version with every poll. The server answers with the range it supports.
  - An agent that is too old is told so, and shown as a problem. It keeps backing up on its cached config.
  - So a server can be upgraded before its agents, and agents one by one.
- The lab installs the packages instead of copying binaries. That tests them on Ubuntu, RHEL and Oracle
  Linux.

**2. Run controls.**
- A backup that fails is retried after 5, then 15, then 45 minutes, as long as the retry starts before the
  job's next run. Each attempt is a run in the history.
- An optional upload limit per job, in KiB/s, maps to restic's `--limit-upload`. It is a structured field,
  not a free-form argument.

**3. Size.**
- Once a day the server lists each host's prefix with its object versions: one listing gives the current
  bytes and the old versions' bytes.
- Metrics carry both, so Prometheus shows growth. The host page shows them too.

**4. Escrow at host add.**
- `server.yaml` gets an `age` recipient (a public key); the private key is kept offline.
- At host add, and whenever a password changes, the server writes `escrow/<fqdn>.age` to the bucket: the
  host's repository password, encrypted to that key.
- Then a lost PostgreSQL loses no repository, even for a host added after the last manual escrow export.
- The maintenance key needs write access to `escrow/`, so the storage contract grows a check.

**5. Compromise.**
- `holdfast host rotate <fqdn>` adds a new restic password to the repository, removes the old one, revokes
  the agent's token, and issues a new enrollment code.
- The runbook says what else to do:
  - make a new S3 key at the S3 server;
  - rebuild or clean the host, and enroll it again;
  - find the last good snapshot from the snapshot list and the changes.

**7. Decommission.**
- Only for a removed host, and only by an admin, who types the FQDN.
- It waits 7 days, like every other deletion, and can be cancelled meanwhile.
- Then the server deletes every object under the host's prefix with the maintenance key. S3's old versions
  stay until the lifecycle rule expires them. The host's page says when the data is really gone.

**8. Dumps.**
- A dump job runs a command on the host and pipes its output into `restic backup --stdin-from-command`, as
  one file in the snapshot.
- **The commands are defined on the host, in `agent.yaml`, not on the server** (Q5). A job names one.
  Otherwise anyone who may change jobs could run any command as root on every host.
- A restore of a dump writes the file into a new directory. Loading it into the database is the admin's
  job, on the host.

**9. Restore to another host.**
- **The usual case is a rebuilt host with the same FQDN.** Adding the FQDN again brings the host back with
  its repository (plan-m4 Q7). The runbook covers it.
- **Restoring host A's snapshot onto a different host B** (Q6):
  - B's agent gets A's repository password and a read-only key for A's prefix, which the admin makes, for
    that one restore;
  - B's `agent.yaml` must allow restores from other hosts, which is off by default;
  - B never gets write access to A.

**10. Rollback.**
- For a repository that was damaged or emptied behind Holdfast's back (decision 032's "disappeared"), or by
  a mistaken Prune now.
- The server shows what rolling the prefix back to a chosen time would change: objects brought back, objects
  set aside.
- After an admin types the FQDN, it makes each object's version from that time the current one again. It uses
  copies and delete markers, so nothing is lost for good, and the rollback can be rolled back too.
- The host's agent is paused for it. Then `restic check` runs, and the snapshot list is read again.

## 3. Tests

- **e2e:**
  - an upgrade from the previous agent version to this one, and an agent too old for the server;
  - a retry within the gap, with the upload limit;
  - the escrow file decrypts with the age key and opens the repository with plain restic;
  - rotate, then a backup with the new password;
  - decommission, with its wait and its cancellation;
  - a PostgreSQL dump restored and loaded;
  - a restore onto another host with an admin-made read-only key;
  - a rollback that brings back snapshots deleted behind Holdfast's back, after which `restic check` passes.
- **Lab:** the packages installed on Ubuntu, RHEL and Oracle Linux; then the existing fidelity test, and the
  dump test with each OS's PostgreSQL.

## 4. Order

Items 1 to 7 first: Holdfast becomes installable and operable for real. Then items 8 to 10, the recovery
features. That split is Q1.

## 4a. Progress

| # | Item | State |
| --- | --- | --- |
| 1 | Packages and upgrades | not started |
| 2 | Run controls | done: retries at 5, 15 and 45 minutes within the gap, `Nice=10` and `IOSchedulingClass=idle` in the unit, `--limit-upload` (spec §4.1) |
| 3 | Size | done: a daily listing with versions; the host page, `host show` and `holdfast_repository_bytes{versions}` |
| 4 | Escrow | done: `escrow.age_recipient`; `escrow/<fqdn>.age` at host add, rotate and backfill; B012; the `escrow` problem; `holdfast_escrow_current` and `holdfast_escrow_configured` |
| 5 | Compromise | done: `holdfast host rotate` (spec §3.5a), [runbooks/compromised-host.md](runbooks/compromised-host.md) |
| 6 | Alerting | done: [alerting.md](alerting.md), a mailer and a dead man's switch in `deploy/alerting/`, new rules with promtool tests |
| 7 | Decommission | done: `holdfast host decommission` with `--confirm` and `--cancel`, the new admin permission `host.decommission`, the really-gone date (spec §3.5) |

Choices made inside the accepted proposals, for you to check:
- **Rotate needs `config.storage`**, like the S3 key it sits next to. **Decommission is a new admin
  permission, `host.decommission`**, so `host.remove` still never deletes data.
- **Rotate revokes the token by moving the host back to waiting for enrollment.** The host shows the `enroll`
  warning until it enrolls with the new code. The old password stops working once its key is removed, so the
  host's own backups fail meanwhile, which is the point.
- **A decommissioned host added again starts over** with a new repository password, and needs a new S3 key.
  A host whose decommission is waiting cannot be added back until it is cancelled.
- **B012 is tested only when a recipient is set**, so a site without escrow does not fail its storage test.
- The lab is not yet set up for escrow (its maintenance policy and `server.yaml`); that comes with item 1,
  when the lab installs the packages.

## 5. Questions

**Q1. One milestone, or two?** Ten items is a lot for one PR.
- *Proposal:* split them. M5 is items 1–7, operating Holdfast. A new M6 is items 8–10, recovering with it.
  Off-site copies become M7. CLAUDE.md's milestone table changes to match.
- *The alternative:* all ten in M5.

**Q2. Packages: signed, and in a repository?**
- *Proposal:* M5 builds `.deb` and `.rpm` files with checksums, attached to each release, unsigned.
  Signing, and an apt or yum repository, come once there is a key for the project and somewhere to host it.
- *The alternative:* sign them in M5. That needs a GPG key and a decision about who holds it.

**Q3. The age key for escrow (plan-m1 Q5's format).**
- *Proposal:* one recipient in `server.yaml`, set by the admin. Without one, nothing is written and the
  host page warns. The file holds the FQDN, the repository URL and the repository password, but not the S3
  key: the admin makes those, and can make a new one.
- *The alternative:* the S3 key in the file too.

**Q4. Retries.** *Proposal:* 3 retries, at 5, 15 and 45 minutes, only while before the next run. They are
counted, and a job whose retries all failed is "failed", as today. The numbers are fixed, not settings.

**Q5. Dump commands on the host.**
- *Proposal:* `agent.yaml` names each command, such as `postgresql: [/usr/bin/pg_dumpall]`, run as a
  given user. A job says `dump: postgresql`. Holdfast ships examples for PostgreSQL and MySQL/MariaDB.
- *The alternative:* the server sends the command. It is simpler, but then a job editor is root on every
  host.

**Q6. Restore to another host.**
- *Proposal:* as in §2, with a read-only key the admin makes and an opt-in in the target host's
  `agent.yaml`. Admin only (`restore.in_place`).
- *The alternative:* only the same-FQDN rebuild, and a runbook for the rest using plain restic and the
  escrow bundle.

**Q7. Decommission's wait.** *Proposal:* 7 days, like every other deletion (CLAUDE.md), cancellable, with
the typed FQDN. The mockup's server-and-vault page said 14 days. Say if you prefer that.

**Q8. Rollback's scope.**
- *Proposal:* one host's whole prefix, to one point in time, by an admin with the typed FQDN, after the
  preview. The agent is paused meanwhile.
- *The alternative:* leave it to the storage admins and a runbook, since only they can read S3's version
  history in every S3 server.
