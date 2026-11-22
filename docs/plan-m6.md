# Plan for M6: recovering with Holdfast

Status: accepted, 2026-11-22. You answered the questions in §5 with "Just do whatever, we can always revise", so
every proposal stands as written. Q1 changes CLAUDE.md's 7-day rule to name a repository rollback as a third admin
exception; decision 038 records it. Q9's "done when" goes into CLAUDE.md.

M6 is plan-m5's items 8–10 (plan-m5 Q1): dumps, restore to another host with the bare-metal runbook, and rolling a
repository back from S3's old versions. plan-m5 Q5, Q6 and Q8 were answered there ("Go"), so their proposals stand
here: dump commands are defined on the host; a restore onto another host uses a read-only key the admin makes and an
opt-in on that host; a rollback covers one host's whole prefix, to one point in time, by an admin, after a preview.
This plan adds the details, and asks what those answers left open.

## 1. What M6 holds

| # | Feature | From |
| --- | --- | --- |
| 1 | **Dumps**: a database or anything else a command prints, backed up with `restic backup --stdin-from-command` | 030 §15, plan-m5 Q5 |
| 2 | **Restore to another host**: host A's snapshot onto host B, with a read-only key for A | 030 §15, plan-m5 Q6 |
| 3 | **The bare-metal runbook**: a host rebuilt from nothing, with Holdfast up and with Holdfast gone | 030 §15 |
| 4 | **Roll a repository back** to a point in time, from S3's old versions | 030 §15, decision 032, plan-m5 Q8 |

Not in M6: off-site copies (M7), rebuilding Holdfast itself (M7, OQ3).

A note on the name. Decision 013, "no rollback", is about config revisions, and stays. Item 4 rolls a repository's
files back, which is a different thing. The spec's sentence in §3.2 becomes "there is deliberately no rollback of
the config".

## 2. Design in brief

### What restic does, checked against 0.19.1

- `restic backup --stdin-from-command --stdin-filename db.sql -- <command>` stores the command's output as one
  file, `/db.sql`. **A command that exits non-zero, or is not there, fails the backup, and no snapshot is made.**
  So a half-written dump is never saved as if it were whole.
- **restic passes its own environment to the command.** Holdfast puts the host's S3 key in restic's environment.
  So the dump command must not inherit it (§2.1).
- **A restore from a repository the key cannot write to needs `--no-lock`**: without it, restic fails to create
  its lock. With it, the restore works.

### 2.1 Dumps

- **The commands are in the host's `agent.yaml`, and the server only names one** (plan-m5 Q5):

  ```yaml
  dumps:
    postgresql:
      command: [/usr/bin/pg_dumpall, --clean]
      user: postgres            # run as this user; default root
      filename: postgresql.sql  # the file's name in the snapshot; default <name>.dump
  ```

- A job is either paths or a dump: `holdfast job set web01 pg --dump postgresql --daily 01:00 …`. Schedule,
  retention, retries, the upload limit and the checks are the same as for any job.
- **The agent runs it with a clean environment**:
  `env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LANG=C.UTF-8 runuser -u postgres -- /usr/bin/pg_dumpall --clean`,
  so the command never sees restic's S3 key or password file. `runuser` is in util-linux on every distribution
  Holdfast supports.
- The agent reports the names of its dumps, not their commands, so the server can check a job: **D001** (error)
  when the host's agent has no dump of that name.
- **A restore of a dump writes the file into a new directory** (plan-m5 §2), never in place. Loading it into the
  database is the admin's job, on the host. Files, Changes and Find show the one file, as for any snapshot.
- Holdfast ships examples for PostgreSQL (`pg_dumpall` as `postgres`) and MySQL/MariaDB (`mariadb-dump
  --all-databases --single-transaction` as root, over the socket).

### 2.2 Restore to another host

- An admin restores host A's snapshot onto host B: `holdfast restore A <snapshot> --to-host B --s3-key-file
  <read-only key for A>` (and the same in the UI and the API).
- **The admin makes the read-only key** at the S3 server: read and list under `repos/A/`, nothing else. Holdfast
  tests it before it uses it: **B014** when it cannot read or list A's prefix, **B015** when it can write or
  delete there. A key that fails is refused.
- **B must opt in**: `restore.allow_other_hosts: true` in B's `agent.yaml`, off by default. B's agent refuses
  otherwise, and says why.
- The server sends B's agent a restore command with A's repository URL, A's password and the read-only key. The
  command is sealed in the database like other credentials, deleted once B has acknowledged it, and never in a log
  or an audit entry. B writes them to root-only files for that one restore, runs `restic --no-lock restore`, and
  removes them.
- B never gets write access to A. B's root does learn A's repository password; the confirmation says so, and the
  runbook says to delete the read-only key afterwards, and to rotate A's password if B is not trusted with it.
- The target is a new directory or another directory, within B's bounds, not in place: an in-place restore takes
  a safety backup of what it replaces, which belongs to a job, and B has no job for A's files.
- It is in both hosts' history and in the audit log.

### 2.3 The bare-metal runbook

`docs/runbooks/bare-metal.md`, two paths:
- **Holdfast is up, and the host comes back with the same FQDN.** Install the OS and the packages, get an
  enrollment code (`holdfast host enroll-code`, or add the host again if it was removed: its repository comes
  back with it), enroll, then restore what is needed. This is plan-m4 Q7's "rebuilt host".
- **Holdfast is gone too.** From a rescue system: partition and mount the new disk, take restic from the
  `holdfast-restic` package or restic's release, decrypt the host's escrow file with the offline age key, and run
  `restic restore` with a read-only key the storage admin makes. Then the bootloader, `fstab` and, on EL, an
  SELinux relabel.

### 2.4 Roll a repository back

- `holdfast host rollback A --to 2026-11-20T03:00Z` shows what it would change; `--confirm A` (the FQDN, typed)
  does it. The UI shows the same preview and asks for the FQDN. Admin only.
- **The preview** lists the prefix with its versions. For each object it takes the version that was current at
  that time, and compares it with today's:
  - **brought back**: there then, gone or different now;
  - **set aside**: there now, not there then;
  - by kind (`data/`, `index/`, `snapshots/`, `keys/`, `config`), with the snapshot IDs that come back and those
    that are set aside. It also lists when snapshots were deleted (the delete markers' times under `snapshots/`),
    to help pick the time.
- **Doing it** uses only copies and delete markers: a version from then is copied over the current one, and an
  object that was not there then gets a delete marker. No version is ever deleted. So the rollback can itself be
  rolled back, to a time just before it ran.
- `locks/` is left alone. Everything else is rolled back, `keys/` and `config` included: a repository that was
  made again after that time needs its old keys back to open its old data.
- The time must be within the lifecycle rule's days (B011), or the old versions may be gone. The preview says if
  any version it needs is missing, and then it is refused.
- The repository as it was at any moment is what a crash at that moment would have left. restic is built to
  survive that, and `restic check` afterwards confirms it.
- The steps: the host's agent is paused (Q3), the server rolls the prefix back, runs `restic check`, reads the
  snapshot list again (snapshots that came back are no longer "disappeared"), and the agent resumes. Snapshots set
  aside get tombstones marked **Deleted by Holdfast (rollback)**.
- It needs two more permissions on the maintenance key: `s3:GetObjectVersion` (to copy a version), and
  `s3:ListBucketVersions` (M5 added it for size).
- A rollback past a prune brings back what the prune removed. The next daily run forgets it again if no retention
  in force during the last 7 days keeps it, by the usual rules (spec §3.4). The preview says which snapshots that
  would be.

### 2.5 The protocol goes to 2

Dump jobs, restores from another host and pausing need an agent that knows them. An agent that speaks protocol 1
would back up a dump job as a job with no paths. So M6's agent speaks protocol 2, and the server:
- refuses a dump job on a host whose agent speaks 1 (**D002**, an error: "upgrade the agent first"), and refuses a
  restore from another host, or a rollback's pause, for such an agent, with the same reason;
- still accepts protocol 1 agents for everything else (`MinProtocol` stays 1).

## 3. Tests

- **e2e**, against restic and MinIO:
  - a PostgreSQL dump of the e2e database: backed up, restored into a new directory, loaded into a fresh database,
    and compared; a dump command that fails gives a failed run and no snapshot; the dump command sees no `AWS_*` and
    no `RESTIC_*` variable;
  - a restore onto another host with an admin-made read-only key; a key that can write is refused (B015); a host
    without the opt-in refuses;
  - a rollback that brings back snapshots deleted behind Holdfast's back (with MinIO's root, as a storage admin
    could), after which `restic check` passes and the "disappeared" alert clears; then the rollback rolled back;
  - a protocol 1 agent cannot get a dump job (D002).
- **Lab**: the dump test with each OS's own PostgreSQL on ub24, el9 and ol9 (backed up, restored, loaded), and the
  bare-metal runbook's first path on one VM (Q8).

## 4. Order

Dumps first (the smallest, and the protocol change), then restore to another host, then rollback, then the runbook
and the lab.

## 5. Questions

**Q1. Rollback and the 7-day protection (a contradiction with CLAUDE.md).** CLAUDE.md says nothing destructive
bypasses the 7 days except an admin's Prune now or Delete now. A rollback sets aside everything written after the
chosen time, at once.
- *Proposal:* a rollback deletes nothing: every object it sets aside stays in S3 as an old version, as long as the
  lifecycle rule keeps it, and rolling the rollback back brings it all back. Waiting 7 days would make it useless as
  a recovery tool. So treat it like Prune now: admin only, a preview, the FQDN typed, audited. CLAUDE.md's rule
  then names it as a third exception.
- *The alternative:* a rollback waits 7 days like any deletion, or may only bring objects back and never set any
  aside (then a damaged repository stays damaged).

**Q2. Rollback and the repository password.** If the password was rotated after the chosen time, the rolled-back
`keys/` holds the old password's key, which the server no longer has.
- *Proposal:* refuse a time before the host's last rotation, and say why; a rollback past it is done by hand, with
  the escrow file of that time (S3 keeps its old versions too) and plain restic.
- *The alternative:* keep today's key files while rolling back the rest. That breaks when the repository was made
  again after that time, because today's keys then open the wrong master key.

**Q3. Pausing the agent for a rollback.** The agent backs up on its own, so the server has to ask it to stop.
- *Proposal:* two new commands, pause and resume. A paused agent starts no run; a slot that comes meanwhile is
  reported as skipped, "paused for a rollback". The rollback waits until the agent reports that it is paused and
  idle. A pause ends by itself after 2 hours. An agent that does not answer within 5 minutes means no rollback,
  unless the admin says the host is down (`--agent-down`, audited) or the host is removed.
- *The alternative:* no pause, and the admin stops the agent on the host by hand.

**Q4. Rollback's permissions.**
- *Proposal:* a new admin permission, `repository.rollback`. `s3:GetObjectVersion` is tested by the preview, not by
  the daily storage test: a new storage check would fail every existing installation on upgrade until its policy
  changed. `docs/storage.md`'s example policy grants it.
- *The alternative:* a new storage check, B013, for every host, every day.

**Q5. Dumps.**
- *Proposal:* as in §2.1: `agent.yaml`'s `dumps` with `command`, `user` and `filename`; a job's `dump`; the clean
  environment; the agent reports dump names only; D001 and D002.
- *Open details:* whether a dump may set environment variables of its own (`env:` in `agent.yaml`, for
  `PGHOST` and the like). *Proposal:* yes, only those it lists, on top of the clean environment.

**Q6. The e2e tests need PostgreSQL's client tools**, which the Go image lacks.
- *Proposal:* `make e2e` builds a small image once, from the pinned `golang:1.26` plus Debian's
  `postgresql-client-17`, and reuses it.
- *The alternative:* install the package at the start of every run (slower, and needs the network each time), or
  test the dump with a fake command only, and leave PostgreSQL to the lab.

**Q7. Restore to another host.**
- *Proposal:* as in §2.2: `restore.in_place` on both hosts (admin); the key tested with B014 and B015; the secrets
  sealed in the command and deleted once acknowledged; `restore.allow_other_hosts` on B; a new or another
  directory, not in place.
- *The alternative:* also in place, with the safety backup going into B's own repository under a job of its own.

**Q8. How far the bare-metal runbook is tested.**
- *Proposal:* the lab walks the first path on one VM: its agent's state and the fixture removed, the packages
  installed again, the host enrolled with a new code, and the fixture restored and compared. The second path
  (rescue system, Holdfast gone) is written, not automated.
- *The alternative:* also automate the second path, booting a lab VM from a rescue image.

**Q9. M6's "done when" in CLAUDE.md.**
- *Proposal:* "The dump, restore-to-another-host and rollback e2e tests pass; the lab's dump test passes on the
  Ubuntu, RHEL and Oracle Linux VMs; and the bare-metal runbook's first path is walked in the lab."

**Decisions to write once answered:** 037, dump commands are defined on the host (plan-m5 Q5); 038, rolling a
repository back from S3's versions (Q1–Q4).
