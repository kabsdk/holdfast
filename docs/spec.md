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
  - `restic check` takes an exclusive lock, and a backup that meets it waits or fails. So each agent
    checks its own repository: once a day, right after a backup, inside its one-restic-at-a-time loop,
    reading a rotating 1/30 of the data (decision 030). `holdfast check` asks every agent to check now.
  - The server tests each host's storage contract once a day. It checks a repository itself only before
    it prunes it.
  - A backup or a check that runs past its timeout is stopped and reported as failed, so the host's other
    jobs can run. The timeout is the schedule's longest gap, and at least an hour.
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

- There is no standby (decision 030). Agents back up without the server, so an outage costs only
  reporting and maintenance.
- Instead, there is a nightly PostgreSQL dump and config export to site B, the sealing key is in escrow,
  and a tested runbook rebuilds the server.
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
- **The floor is in storage, not in Holdfast** (decision 030). No Holdfast key can delete an object
  version for good or change the bucket's settings. So whatever is deleted can be recovered until the
  bucket's lifecycle rule expires its old versions. That holds against a compromised host, a compromised
  server and Prune now. The 7-day protection (§3.4) is Holdfast's own rule on top of it.
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
  and the full config (hosts and jobs).
- Any two revisions can be diffed.
- Each revision also stores the warning codes that were accepted for it (§4.5).

**There is deliberately no rollback.** To undo a change, edit the config and save it. That save passes the
same checks as any other change. A rollback would be a bulk edit in disguise, and it could silently cut
retention. See [decision 013](decisions/013-no-rollback.md).

### 3.3 Interface parity and audit

- The options in the UI, the API and the CLI are the same, with very few exceptions. Any exception is listed
  here:
  - **Escrow export** exists only on the server's root socket (`holdfast escrow export` as root), never over
    the network.
  - **Settings in `server.yaml`** are changed by editing the file as root. The UI and the API only show them,
    without secrets.
  - **Login** differs by client: a browser is redirected to the provider, the CLI shows a device code
    (decision 034). Both end in the same kind of session.
- The API comes first. The UI and the CLI are thin clients of the HTTP API, and all logic, validation and
  checks live behind it (decision 028).
- Every action is recorded in the host's history and in the audit log.

### 3.4 Retention safety

These rules hold whatever the config says.

**High-water mark.** Maintenance never prunes a snapshot that any policy in force during the last 7 days
would keep.

**The daily run** (plan-m3 §3). Once a day per host, the server:
1. takes each job's retention policies that were in force during the last 7 days, from the revisions;
2. asks restic what each one keeps: `restic forget --dry-run --json --tag job=<id> --group-by host`, with
   the policy's flags (decision 030);
3. keeps the union, and forgets the rest by exact snapshot ID. It forgets only snapshots its agents
   reported; a snapshot Holdfast did not make is left alone (decision 032);
4. runs a structural `restic check`, then `restic prune`. A failed check blocks the prune; the run is
   recorded as failed and raises `HoldfastRetentionFailing`.

- It runs in the middle of the host's longest quiet stretch, so it rarely meets a backup, and it waits for
  a backup's lock rather than failing. A run missed while the server was down waits for the next day's slot.
- Only the server runs forget and prune, with its maintenance key; agents never do.
- `holdfast retention run <fqdn>` starts the same run now, with the same protection.
  `maintenance.retention: off` in `server.yaml` turns the daily run off.
- Removing a job never deletes data (§3.5): the daily run covers the jobs in the current config only.

**Pending.** A retention cut becomes pending for 7 days, however the cut was made. `holdfast pending
<fqdn>` (`GET /api/v1/hosts/{fqdn}/pending`) shows each waiting snapshot, the revision whose policy still
keeps it, and when the daily run may forget it.

**Keep longer.** `holdfast job keep-longer <fqdn> <job>` saves a new revision in which the job has an older
retention again: by default the newest one in force during the last 7 days that differs from today's. It
is a normal save, with the normal checks.

**Prune now** lets an admin skip the 7-day wait. It forgets what the job's *current* policy drops, ignoring
the union, then checks and prunes.
- Only admins can use it.
- It needs a confirmation page that shows:
  - how many snapshots will be deleted and how many kept;
  - the date range;
  - the full list of snapshots, paginated and filterable by month, with every ID clickable;
  - the exact restic commands that will run: `forget -- <ids>`, `check`, `prune`.
- The admin confirms by typing the host's short name (`holdfast prune-now <fqdn>` shows the page;
  `--confirm <short name>` goes ahead).
- It is recorded in the host's history and in the audit log.
- The deleted snapshot IDs can still be found in search, marked **Deleted**.

**Delete a snapshot** (decision 031)
- An admin can delete one snapshot, with restic's `forget <id>`. It is a UI action, an API endpoint, and
  `holdfast snapshot delete <fqdn> <id>`.
- It becomes a pending deletion for 7 days, which can be cancelled (`--cancel`). **Delete now** (`--now
  --confirm <short name>`) skips the wait, with the same confirmation as Prune now.
- It never touches site B. The vault marks its copy **Deleted in primary**, and a vault admin can choose
  **Delete here too**. That deletion follows the same rules in B.
- The snapshot page shows when the data is really gone in each site. Space is freed at the next prune, and
  S3 versioning keeps the old files until the bucket's lifecycle rule expires them. The daily storage test
  reads that rule's days (B011), so the date is the prune's date plus those days.

**Past prunes** (OQ2). `holdfast prunes [<fqdn>]` lists each forget and prune: when, the daily run or Prune
now or Delete now, who, the snapshots forgotten, what restic said the prune freed, and when the data is
really gone. Snapshots Holdfast deleted keep tombstones marked **Deleted by Holdfast**; they never count as
disappeared (§3.6).

### 3.5 Removing and decommissioning

- Removing a job or a host never deletes data (plan-m4 Q7).
  - A removed job leaves the config. Its snapshots stay, and the daily run no longer touches them.
  - A removed host is retired: its agent's token is revoked, and its repository stays and can still be
    browsed. Adding the same FQDN again brings it back, with its repository, after a new enrollment.
- Deleting data is a separate **decommission** action. It requires typing the FQDN, it has a grace period,
  and it is audited.

### 3.6 Changes made outside Holdfast

See decision 032.
- **Holdfast's own files on a host are repaired, and the repair is recorded:**
  - the cached host config, which the server sends again;
  - the credential files, which the agent checks before each run and fetches again if they changed.
- **Changes to a repository are shown in red and alerted, never undone.** From M2:
  - a snapshot Holdfast did not make;
  - a snapshot that disappeared without Holdfast deleting it, with the date until which it can be
    recovered and an offer to roll back from S3's versions;
  - a restic password added outside Holdfast.
- **restic:**
  - the agent's package ships the restic Holdfast is tested with, and the agent never downloads
    anything;
  - a host that reports another version gets a warning and, after an hour, an alert.

---

## 4. Schedule, retention and protection

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

### 4.4 Library (cut)

The library, profiles and "Jobs with the old values" were cut (decision 030). The section number is kept
so that references to §4.5 stay valid.

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
| R103 | The longest gap between runs is more than 1 day. The message gives the worst-case data loss, spread included. |
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
| B009 | The maintenance key can delete an object version for good. |
| B010 | The maintenance key can change the bucket's settings: versioning, the lifecycle rule or the bucket policy. |
| B011 | No enabled lifecycle rule expires noncurrent versions under `repos/`. |

- The accepted codes are stored in the revision and in the audit log.
- The CLI takes them explicitly, for example `holdfast job set … --accept R102,R104`. Without them it
  refuses the change and prints the same explanation as the UI. The API takes them as `accept`, and
  answers a refused save with 422 and every finding.
- `holdfast job check` (`POST /api/v1/hosts/{fqdn}/jobs/{job}/check`) runs the checks without saving.
- S002 needs at least 5 finished runs. R101, R102 and R103 are estimates from the schedule and the
  windows, so their messages say "about".
- restic decides what each policy keeps (decision 030). Holdfast runs `restic forget --dry-run` for the
  old and the new policy. A change is a retention cut (R104) when the new policy keeps a strict subset of
  what the old one keeps. Removing a tier is therefore not a cut if a finer tier keeps the same snapshots.
  A job with no snapshots yet has nothing to cut. If restic cannot be asked, the change counts as a cut.

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
| Changes | `restic diff --metadata` against the previous snapshot or any older one: what restic's diff shows, and nothing more. That means the changed paths (added, removed, content changed, metadata only, type changed) and their counts. It never shows contents or line diffs, as restic's diff does not. To compare the lines of a file, **Restore both versions to diff on the host** (§5.4) and run the `diff -u` it prints, on the host. |
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

**The host sets the bounds** (decision 030). The agent's own config, which only root on the host can
change, decides what a restore may do. A compromised server or admin login is then not root on every
host.
- By default, restores go into `/var/restore` only.
- In place, or into another directory, only on hosts that opt in.
- Only snapshots this agent made.

**Running it**
- A **dry run** is available. It reports what would be restored, and writes nothing.
- The request gives either what was left out (`--exclude`) or only what to restore (`--include`, for
  **Restore only this**), not both. restic always runs with `--sparse --verify`.
- The restore runs on the host, through its agent, in the agent's one-restic-at-a-time loop. Its result,
  with counts and the target directory, is in the host's history and in the Restores tab (§5.5).
- A restore that the host has not picked up within 24 hours is cancelled, and the cancellation is reported.

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
2. Give the host's S3 key (decision 029, [storage.md](storage.md)), and optionally its first job.
3. Saving creates a revision.
4. Holdfast shows a single-use enrollment code, valid for 15 minutes, and the install command.
5. Progress is shown: **waiting → agent connected → first backup done**.

---

## 6. Access control

### 6.1 Login

- Login is through OIDC (Authelia, or any provider with the code flow and the device flow). The server is
  the only OIDC client (decision 034):
  - a browser logs in with the code flow and gets a session cookie;
  - `holdfast login --server <url>` shows a device code to enter at the provider, and gets a session token.
- Groups come from the groups claim. This works with or without LDAP. They are read at login.
- A browser session lasts 12 hours and ends after an hour without use; a CLI session lasts 7 days. An admin
  can end a user's sessions (`holdfast access end-sessions <user>`).
- People use the UI and the API on their own listener, `ui.listen`, with a certificate browsers trust. Agents
  use the agent listener.
- Root on the server uses the root-only socket, and is admin on every host.
- The second factor is the OIDC provider's job, not Holdfast's.
- There are no sudo checks on the hosts.

### 6.2 Roles

There are three roles. Each has a flat set of permissions, and roles do not inherit from each other.

| Permission | viewer | editor | admin |
| --- | :-: | :-: | :-: |
| `view`: hosts, jobs, schedule, retention, status, history, alerts | ✓ | ✓ | ✓ |
| `browse`: file names and metadata (Files, Changes, Find, restore targets) | | ✓ | ✓ |
| `backup.run` | | ✓ | ✓ |
| `restore.new_dir` | | ✓ | ✓ |
| `config.jobs` | | ✓ | ✓ |
| `restore.in_place` | | | ✓ |
| `retention.cut` (includes Prune now) | | | ✓ |
| `snapshot.delete` (includes Delete now) | | | ✓ |
| `config.access` | | | ✓ |
| `config.storage` | | | ✓ |
| `maintenance`: acknowledge changes made outside Holdfast, `holdfast check` | | | ✓ |
| `host.remove`: remove a host or a job (never deletes data) | | | ✓ |

A user can have several bindings. They combine per host. **A host you have no binding for does not exist
for you:** it is not listed, not searchable and not counted (decision 033). Asking for it answers "not
found". Revisions and the audit log are cut down to your hosts. Nobody sees file contents.

Actions the table does not name (plan-m4 Q6):
- adding a host, replacing its S3 key and issuing an enrollment code need `config.storage`;
- a restore into another directory needs `restore.in_place`;
- saving a job needs `config.jobs`, and also `retention.cut` when it accepts R104;
- `holdfast retention run` needs `retention.cut`;
- the restore paths in the Restores list are file names, so they need `browse`.

There are no custom roles (decision 035).

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
ui:
  listen: ":443"
  public_url: https://holdfast.example.internal
  tls: {cert_file: /etc/holdfast/ui.crt, key_file: /etc/holdfast/ui.key}
auth:
  oidc:
    issuer: https://auth.example.internal
    client_id: holdfast
    client_secret_file: /etc/holdfast/oidc-client-secret
    groups_claim: groups
  bootstrap_admins: []   # usernames that are admin everywhere, for the first login; each use is audited
```

### 6.5 File contents

No file contents reach the UI, the API or the CLI. Only names and metadata do (§5.3). `content_view` was cut
(decision 030). A restore runs on an agent and writes to that host.
