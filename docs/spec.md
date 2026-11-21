# Holdfast specification

Status: draft for review, 2026-11-21.

This document describes how Holdfast behaves. When behaviour changes, this file changes in the same PR.
The reasons behind the decisions are in [decisions/](decisions/). Topics that are not decided are in
[open-questions.md](open-questions.md), and nothing in this document decides them. The working rules are in
[../CLAUDE.md](../CLAUDE.md).

---

## 1. What Holdfast is

Holdfast is a free, open-source, centrally managed backup platform for Linux hosts running Ubuntu,
Red Hat Enterprise Linux or Oracle Linux. It is a control plane over **restic**, in the same way that
Clusterlane is a control plane over keepalived and HAProxy.

There are no paid tiers and no feature gating.

### 1.1 Scope

- **restic only.** Holdfast configures, schedules, monitors and restores restic backups.
- **Host identity is the FQDN.** Each host has one restic repository on S3 under the prefix
  `repos/<fqdn>`. Any S3 server that meets [storage.md](storage.md) will do: Ceph RGW, MinIO, AWS and
  others.
- **Jobs share the host's repository.** Every job on a host writes to that host's repository, and each
  snapshot is tagged `job=<id>`.
- **Only Holdfast's own backups.** The console shows only the backups that Holdfast makes itself. It does
  not show "watched only" backups made by other tools.

### 1.2 Out of scope for now

- **Kubernetes**, including PostgreSQL on CloudNativePG. Those clusters keep CloudNativePG's own Barman
  Cloud plugin backups.
- **Database files on hosts.** A file copy of a running database is not a backup. For example,
  `/var/lib/pgsql` is excluded.

### 1.3 Engines and capabilities

A job is an **engine** behind one interface. restic is the only engine today. A PostgreSQL-native engine
(physical base backups, WAL archiving, point-in-time recovery) is planned for later.

Each engine declares which capabilities it supports. The UI, API and CLI show only what the job's engine
supports.

| Capability | restic engine |
| --- | --- |
| identity | yes |
| schedule / run | yes |
| freshness | yes |
| restore points | yes |
| browse / changes | yes |
| restore | yes |
| retention / prune | yes |

---

## 2. Architecture

### 2.1 Components

**Agent** (`holdfast-agent`, Go, one per host, runs as root)
- Has a local scheduler and runs restic itself.
- Spools events to disk until the server has acknowledged them.
- Connects to the server by HTTPS long-poll. The agent always dials out.
- Keeps running, and keeps backing up, while the server is down.
- Reports the config revision it has applied, which is how drift is detected (§3.1).

**Server** (`holdfast-server`)
- The configurator.
- The maintenance leader: it runs forget, prune and check.
  - `restic check` takes an exclusive lock, and a backup that meets it waits or fails. So the server
    checks each host once a day, at the middle of that day's longest quiet stretch, between the end of one
    run's spread and the start of the next.
  - A host's view shows when its next check is.
  - A check that was interrupted is marked so when the server starts again. Its stale lock is then
    removed, and only that lock.
- Keeps its state in PostgreSQL.

**Storage witness** (`holdfast-freshness`)
- Lists the bucket without going through any agent.
- This means "the agent says the backup is OK" and "the bucket has a fresh snapshot" are judged
  separately.

**Prometheus**
- Scrapes the server and the witness. It never scrapes individual agents.
- Alert rules ship with the product. The starting set is
  [poc/deploy/prometheus/holdfast-alerts.yml](../poc/deploy/prometheus/holdfast-alerts.yml).

### 2.2 High availability

- There is a warm standby in site B: a keepalived VIP plus a PostgreSQL streaming replica. There is no
  etcd.
- Only the VIP holder runs maintenance. restic's exclusive lock is a second guard against split brain.
- Agents keep backing up while the control plane is down.

### 2.3 Credentials and storage protection

- Each host has its own repository password and its own S3 key.
- The admin makes the S3 keys with the S3 server's own tools, and gives Holdfast each host's key when the
  host is added. Holdfast never creates S3 users, keys or policies, so it works with any S3 server
  (decision 029).
- A host key may use only its own prefix, and may delete only under its `locks/`. The maintenance key may
  do everything under `repos/`. Bucket versioning is on. [storage.md](storage.md) states the full contract.
- Holdfast tests that contract with plain S3 calls:
  - when a host is added;
  - when its key is replaced;
  - before an enrollment is accepted;
  - in each host's daily check.

  A key that fails is refused, and a host whose storage fails cannot enroll. The storage checks are in
  §4.5.
- Keys are escrowed in an offline escrow bundle.
- Credentials are never shown in the UI.
- There is no raw restic console.

---

## 3. Configuration model

### 3.1 Authority and drift

- Holdfast is the authority for config.
- Drift is detected and shown.

### 3.2 Revisions

- There is no Git and there are no approvers.
- Every save creates a revision in PostgreSQL. A revision holds its number, who made it, when, a message,
  and the full config (hosts, jobs and library).
- Any two revisions can be diffed.
- Each revision also stores the warning codes that were accepted for it (§4.5).

**There is deliberately no rollback.** To undo a change, edit the config and save it. That save passes the
same checks as any other change. A rollback would be a bulk edit in disguise, and it could silently cut
retention. See [decision 013](decisions/013-no-rollback.md).

### 3.3 Interface parity and audit

- The options in the UI, the API and the CLI are the same, with very few exceptions. Any exception is listed
  here. None are listed yet.
- The API comes first. The UI and the CLI are thin clients of the HTTP API, and all logic, validation and
  checks live behind it (decision 028).
- Every action is recorded in the host's history and in the audit log.

### 3.4 Retention safety

These rules hold whatever the config says.

**High-water mark.** Maintenance never prunes a snapshot that any policy in force during the last 7 days
would keep.

**Pending prune.** A retention cut becomes a pending prune for 7 days, however the cut was made. During
those 7 days a one-click **Keep \<longer\> retention** action is available.

**Prune now** lets an admin skip the 7-day wait.
- Only admins can use it.
- It needs a confirmation page that shows:
  - how many snapshots will be deleted and how many kept;
  - the date range;
  - the full list of snapshots, paginated and filterable by month, with every ID clickable;
  - the exact `restic forget --prune` command that will run.
- The admin confirms by typing the host's short name.
- It is recorded in the host's history and in the audit log.
- The deleted snapshot IDs can still be found in search, marked **Deleted**.

### 3.5 Removing and decommissioning

- Removing a job or a host never deletes data. The repository is retired and keeps its last retention.
- Deleting data is a separate **decommission** action. It requires typing the FQDN, it has a grace period,
  and it is audited.

---

## 4. Schedule, retention, protection and library

### 4.1 Schedule

Schedules are structured. The only free-text input is numbers.

| Form | Inputs |
| --- | --- |
| Every N minutes or hours | N, unit, and a start time. The series starts again from the start time each day. |
| Daily | One or more times |
| Weekly | The chosen days and one time |

**Random start spread** is optional and given in minutes. A tick box turns it off, and the number is
remembered while it is off.

These values are derived from the schedule and shown with it:
- the next 5 runs;
- the longest gap between runs;
- the worst-case data loss, which is the longest gap plus the spread;
- the stale alert threshold, which is 2 × the longest gap.

### 4.2 Retention

Retention is expressed only as time windows, which map to restic as follows.

| Holdfast option | restic flag |
| --- | --- |
| Keep every backup for X | `--keep-within X` |
| Keep one per hour for X | `--keep-within-hourly X` |
| Keep one per day for X | `--keep-within-daily X` |
| Keep one per week for X | `--keep-within-weekly X` |
| Keep one per month for X | `--keep-within-monthly X` |
| Keep one per year for X | `--keep-within-yearly X` |
| Always keep the newest N (default 3) | `--keep-last N` |

- Count-based flags are never used. It has been verified that `--keep-daily 14` on a weekly schedule keeps
  14 **weeks** of backups.
- `--keep-last` is the only count, and it acts only as a floor.
- It has also been verified that restic keeps the oldest snapshot when a tier cannot be filled.

### 4.3 Protection item

A protection item is a schedule and a retention bundled together, and they are validated as a unit.

### 4.4 Library

The library is part of every config revision. It holds three kinds of item.

| Item | Contents | Use |
| --- | --- | --- |
| Paths item | Paths and excludes | In a job, each paths item adds to the job's path list. |
| Protection item | A schedule and a retention (§4.3) | |
| Profile | Paths items and one protection item | Used when adding a host |

**Library items are copies.**
- Picking an item copies its values into the job.
- Editing any of those fields in the job switches the job to **Custom**. It stays Custom even if its values
  later match an item again.
- There is no "based on" link and no "reset to".
- **Undo changes** returns the job to the saved revision.

**After a library item is edited**
- Holdfast shows **Jobs with the old values (N)**, with a checkbox for each job.
- Each job is checked against its own config.
- One click updates all the selected jobs in a single revision.
- Warnings must still be accepted (for example R104).
- Retention cuts still go through the 7-day protection (§3.4).

### 4.5 Checks

The UI, the API and the CLI use the same checks engine, and every check has a stable code.

**Errors** block saving.

| Code | Condition |
| --- | --- |
| R001 | A retention tier is finer than the schedule. |
| R002 | A window is shorter than its own period. |
| R003 | A coarser tier adds nothing. |
| R004 | Nothing is kept. |
| R005 | A tier has no duration. |
| S001 | The spread is half the interval or more. |
| S003 | The schedule is invalid. |

**Warnings** must be accepted one by one.

| Code | Condition |
| --- | --- |
| S002 | Runs are scheduled more often than they take (p95 run time). |
| R006 | Keep newest is 0. |
| R101 | The oldest restore point is less than 30 days old. |
| R102 | More than 1000 snapshots would be kept. |
| R103 | The worst-case data loss is more than 1 day. |
| R104 | The change cuts retention. |

**Info**

| Code | Condition |
| --- | --- |
| I001 | There is no yearly tier. |

**Storage checks** test the S3 server, not the config (decision 029, [storage.md](storage.md)). They are
errors that cannot be accepted. A key that fails one is refused, and a host whose storage fails one
cannot enroll.

| Code | Condition |
| --- | --- |
| B001 | The host key cannot write under its prefix, or cannot delete under its `locks/`. |
| B002 | The host key cannot read back or list its prefix. |
| B003 | The maintenance key cannot read what the host key wrote. |
| B004 | The maintenance key cannot write, list or delete under `repos/`. |
| B005 | The host key can delete outside its `locks/`. |
| B006 | The host key can delete, for good, a version of an object outside its `locks/`. |
| B007 | The host key can read or write another host's prefix, or list `repos/`. |
| B008 | Bucket versioning is not enabled. |

- The accepted codes are stored in the revision and in the audit log.
- The CLI takes them explicitly, for example `holdfast job set … --accept R102,R104`. Without them it
  refuses the change and prints the same explanation as the UI.
- A retention cut is detected by what can still be restored, not by the names of the tiers. Removing a tier
  is not a cut if a finer tier covers its window.

---

## 5. Restore and inspection

Restore and inspection behave like restic does on the host.

### 5.1 Navigation

- Path: Host → Job → Snapshot list → Snapshot page.
- Every snapshot ID anywhere in the product is a link.

### 5.2 Job snapshot list

- Shows all snapshots, paginated and filterable by month.
- For each snapshot it shows which rule keeps it, and whether a pending prune will delete it.
- **Find a file in all snapshots** uses `restic find` to show the versions of a file. Each version links to
  the snapshot, opened at that folder.

### 5.3 Snapshot page

| Tab | Behaviour |
| --- | --- |
| Files | `restic ls --long`: names and metadata only. |
| Changes | `restic diff` against the previous snapshot or any older one. Shows counts of added, removed, content-changed and metadata-changed files. Never shows contents. |
| Restore | See §5.4. |

### 5.4 Restore

**Selection**
- Everything is ticked by default. This is an exclusion model: items you untick appear as **Left out**
  chips.

**Target**, one of:
- **A new directory** (the default). Only root can read it. The default path is
  `/var/restore/<date>-<user>-<snapshot>`, created with mode 0700. Restored files keep their owner, mode and
  ACLs.
- **The original location** (in place). Holdfast first backs up what will be replaced, then restores in
  overwrite mode.
- **Another directory.**

**Running it**
- A **dry run** is available.
- The restic command uses whichever of `--include` or `--exclude` gives the shorter list, and always adds
  `--sparse --verify`.

**Per-file actions**
- **Restore only this**.
- **Restore both versions to diff on the host** restores both versions into a directory only root can read,
  and prints the `diff -u` command to compare them.

### 5.5 Restores tab

- Shows the restore history for the whole fleet: who restored what, where to, the result, and a link to the
  snapshot.
- Has a host → job picker that leads to that job's snapshot list.

### 5.6 Search

Search finds:
- hosts;
- jobs;
- snapshot IDs, including deleted ones, which are marked **Deleted**.

### 5.7 Add host

1. Enter the FQDN. It is validated, and a duplicate FQDN is refused.
2. Choose a profile (§4.4).
3. Saving creates a revision.
4. Holdfast shows a single-use enrollment code, valid for 15 minutes, and the install command.
5. Progress is shown: **waiting → agent connected → first backup done**.

---

## 6. Access control and file contents

### 6.1 Login

- Login is through OIDC (Authelia).
- Groups come from the groups claim. This works with or without LDAP.
- The second factor is the OIDC provider's job, not Holdfast's.
- There are no sudo checks on the hosts.

### 6.2 Roles

There are three roles. Each has a flat set of permissions, and roles do not inherit from each other.

| Permission | viewer | editor | admin |
| --- | :-: | :-: | :-: |
| `view` | ✓ | ✓ | ✓ |
| `backup.run` | | ✓ | ✓ |
| `restore.new_dir` | | ✓ | ✓ |
| `config.jobs` | | ✓ | ✓ |
| `restore.in_place` | | | ✓ |
| `retention.cut` (includes Prune now) | | | ✓ |
| `config.access` | | | ✓ |
| `config.storage` | | | ✓ |
| remove host / job | | | ✓ |

A user can have several bindings. They combine per host.

### 6.3 Default bindings

These are part of the normal config.

| Group | Role | Hosts |
| --- | --- | --- |
| `holdfast-admins` | admin | `["*"]` |
| `holdfast-editors` | editor | `["*"]` |
| `holdfast-viewers` | viewer | `["*"]` |

### 6.4 `/etc/holdfast/server.yaml`

Editing this file needs root on the server. Holdfast reloads it, and the reload is audited.

```yaml
auth:
  oidc:
    issuer: https://auth.example.internal
    groups_claim: groups
  bootstrap_admins: []

content_view:
  enabled: false            # default: no file contents in the UI, API or CLI
  require_mode_bits: "0004" # show a file only if its mode has all of these bits
  roles: [admin]
  max_file_size: 1MiB
```

### 6.5 File contents

- By default, no file contents reach the UI, the API or the CLI. Only names and metadata do (§5.3).
- When `content_view.enabled` is true, a file's contents may be shown only if all of these hold:
  - the viewer has one of `content_view.roles`;
  - the file's mode has all of `content_view.require_mode_bits`;
  - the file is no larger than `content_view.max_file_size`.
- Some files are always denied. This cannot be configured:
  - `/etc/holdfast/**`;
  - every job's repository password file.
