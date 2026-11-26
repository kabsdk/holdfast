# Holdfast specification

Status: describes Holdfast 0.1-dev, the development release before 0.1.0 (decision 054), and the main branch.

This document describes how Holdfast behaves. When behaviour changes, this file changes in the same pull request.
The reasons behind the decisions are in [decisions/](decisions/), and the rules every change keeps are in
[../CONTRIBUTING.md](../CONTRIBUTING.md). The guide for people who run Holdfast is
[the documentation site](https://kabsdk.github.io/holdfast/).

---

## 1. What Holdfast is

Holdfast is a free, open-source, centrally managed backup platform for Linux hosts running Ubuntu,
Red Hat Enterprise Linux or Oracle Linux. It is a control plane over **restic**.

There are no paid tiers and no feature gating.

**Who it is for** (decision 042): a small operations team, one to a handful of people, looking after roughly ten to a
few hundred Linux servers, with S3-compatible storage they already run or rent. They want one place to set restic
backups up, see them, restore from them and be alerted, instead of cron jobs and scripts on each host.

### 1.1 Scope

- **restic only.** Holdfast configures, schedules, monitors and restores restic backups. A PostgreSQL job is
  restic too: pg_dump's output goes into restic (§4.6, decision 055).
- **What backs up is an agent** (decision 056): a machine, or a pod that backs up a database server. Its identity is
  its name, set in its own `agent.yaml` and never read from the machine: lower-case letters, digits and hyphens, in
  dot-separated parts of at most 63, at most 253 in all, such as `web01.example.com`, `matrix` or
  `k3s-postgres-main`. This spec still says *host* for an agent in many places: the two are the same.
- **One repository per agent.** Each agent has one restic repository on S3 under the prefix `repos/<agent>`. Any S3
  server that meets [storage.md](storage.md) will do: Ceph RGW, MinIO, AWS and others.
- **Jobs share the agent's repository.** Every job of an agent writes to that agent's repository, and each
  snapshot is tagged `job=<id>`.
- **Only Holdfast's own backups.** The console shows only the backups that Holdfast makes itself. It does
  not show "watched only" backups made by other tools.

### 1.2 Out of scope for now

- **Kubernetes volumes and API objects** (decision 002). A PostgreSQL server in Kubernetes, such as one of
  CloudNativePG, is backed up as any database server, by the agent's container image in a pod (§4.6, decision 055);
  CloudNativePG's own backups can stay beside it.
- **Database files on hosts.** A file copy of a running database is not a backup. For example,
  `/var/lib/pgsql` is excluded. A PostgreSQL database is backed up by a PostgreSQL job, and any other database from
  an export the host writes (§4.6).
- **High availability of Holdfast itself** (decision 042): no clustering, standby, active/passive pair or second
  gateway. One server; a lost one is replaced (§2.2).
- **Windows and macOS machines, service providers backing up many customers, and fleets of thousands** (decision 042).

### 1.3 Engines and capabilities

A job is an **engine** behind one interface. There are two, and both store everything in restic:
- `restic` backs up paths;
- `postgresql` backs up one PostgreSQL database, or a server's roles and other globals, with pg_dump streamed into
  restic (§4.6, decision 055).

A PostgreSQL-native physical engine (base backups, WAL archiving, point-in-time recovery) is planned for later.

Each engine declares which capabilities it supports. The UI, API and CLI show only what the job's engine
supports.

| Capability | restic | postgresql |
| --- | --- | --- |
| identity | yes | yes |
| schedule / run | yes | yes |
| freshness | yes | yes |
| restore points | yes | yes |
| browse / changes | yes | the dump's one file; no Changes |
| restore | into a new directory, another one, or in place | into a new database, in place, or the dump as a file (§5.4) |
| retention / prune | yes | yes |

---

## 2. Architecture

### 2.1 Components

**Agent** (`holdfast-agent`, Go): one per machine, as root; or one per database server, in a container as an
unprivileged user (decision 055)
- Is named in its own `agent.yaml`, `agent_name`, which is required and never read from the machine (decision 056).
  An `agent.yaml` that has the name as `fqdn:` loads, and the agent logs that the key is now `agent_name`. The server
  shows the address the agent last connected from.
- Has a local scheduler and runs restic itself. restic encrypts on the host.
- Runs no command a person defines, and no hook before or after a backup (decision 052). For a PostgreSQL job it
  runs PostgreSQL's own programs from `postgresql.bin_dir` in its `agent.yaml` (pg_dump, pg_dumpall, pg_restore and
  psql), with arguments Holdfast builds, in a clean environment, the password in a file only the agent reads
  (decision 055).
- Spools events to disk until the server has acknowledged them.
- Connects to the server by HTTPS long-poll. The agent always dials out.
- Is known to the server by its own key and a self-signed certificate, which it makes at enrollment and the server
  pins, as SSH pins a host key (decision 041). Every connection is mutual TLS. There is no token and no CA.
- Backs up through the server's gateway: it holds no S3 key. While the server is down it keeps its schedule; a
  backup waits for the gateway, and one that fails is retried as soon as the server answers.
- Reports the config revision it has applied, which is how drift is detected (§3.1).

**Server** (`holdfast-server`)
- The configurator. It serves the API and the UI to people on a listener of its own, `ui.listen`
  (decision 034), and to root on the server on a root-only socket.
- The gateway (decision 041): restic's REST protocol for every host's repository, at `/gateway/v1/<agent>/` on the
  agent listener, in front of the bucket. It knows the host by its certificate, and holds the one gateway key
  (§2.3). It sees only what restic encrypted on the host.
- The maintenance leader: it runs forget, prune and check.
  - `restic check` takes an exclusive lock, and a backup that meets it waits or fails. So each agent
    checks its own repository: once a day, right after a backup, inside its one-restic-at-a-time loop,
    reading a rotating 1/30 of the data (decision 030). `holdfast check` asks every agent to check now.
  - The server tests each host's storage contract once a day. It checks a repository itself only before
    it prunes it.
  - A backup or a check that runs past its timeout is stopped and reported as failed, so the host's other
    jobs can run. The timeout is the schedule's longest gap, and at least an hour.
- The storage witness (decision 045, in place of M1's `holdfast-freshness`): after every backup, and at least once a
  day, it reads the host's repository with restic and the maintenance key, straight from S3, not through the gateway.
  So "the agent says the backup is done" and "the bucket holds it" are judged apart: a snapshot an agent reported that
  the bucket does not hold is the `not_in_bucket` error, and one that was there and went later is `disappeared`
  (§3.6, §5.5a).
- It sends its own alerts, by mail, a webhook or both, and calls a heartbeat URL (decision 046, §5.5a).
- Keeps its state in PostgreSQL.
- Optionally, with `offsite:` in its settings, copies every host's snapshots with `restic copy` into a second bucket,
  under their own retention and the same 7-day protection (§3.7, decision 041). M7's holdfast-vault, a program of its
  own, did this until M8.

There are two programs: holdfast-agent and holdfast-server (decision 041), and the CLI, `holdfast`.

**Prometheus**, optional
- Scrapes the server only, never the agents (decision 009). The server's `/metrics` holds both what the agents
  reported and what the bucket holds.
- Alert rules ship with the product: [deploy/prometheus/holdfast-alerts.yml](../deploy/prometheus/holdfast-alerts.yml).

### 2.2 One server, replaced when lost

- There is one server and no high availability: no standby, no cluster, no second gateway (decisions 030, 042).
- Backups go through the server's gateway (decision 041), so while the server is down no backup is made: agents keep
  their schedule, restic waits and retries, and a failed backup is retried as soon as the server answers. Nothing is
  lost, as the backups are in the bucket. A stopped server sends no alert, so it stops calling `notify.heartbeat_url`,
  and a heartbeat service outside the site raises the alarm; with Prometheus, `HoldfastServerDown` pages (§5.5a).
- A lost server is replaced, not failed over: a new one, given the same bucket, its two keys, the recovery file and
  the nightly export, picks up where the old one was (M9, decisions 040 and 047), following a tested runbook.

### 2.3 Credentials and storage protection

- Each host has its own repository password, and its own key and certificate, which never leave it. It holds no
  S3 key (decision 041).
- **A PostgreSQL connection's passwords** (decision 055), the backup user's and the restore user's, are sealed with the
  credentials key, as repository passwords are, and kept apart from the config: no revision, diff, audit entry or
  answer holds them, only whether one is set. The server sends them only to the agent the connection belongs to, with
  its credentials; the agent keeps them in root-only files in its `credentials_dir`, and gives them to PostgreSQL's
  programs in a pgpass file, never on a command line. They are in the nightly export, still sealed. The off-site
  instance gets the connections without their passwords.
- The admin makes two S3 keys per bucket with the S3 server's own tools, and gives them to the server: the
  gateway key and the maintenance key. Holdfast never creates S3 users, keys or policies, so it works with any S3
  server (decision 029).
- The gateway key may read and write under `repos/`, delete only lock files, and never delete a version. The
  maintenance key may do everything under `repos/` but delete a version. Bucket versioning is on. [storage.md](storage.md)
  states the full contract.
- The gateway decides what each host may do: read and add to its own repository, delete only its own lock files,
  never rewrite a file that exists, and never write the repository's config or keys, which the server made. For a
  restore onto another host, that host may read the snapshot's repository, and only read it, until it reports the
  restore. Every refusal is audited, in the host's history and in the other host's when it was that one's
  repository, and counted in `holdfast_gateway_refusals_total`.
- Holdfast tests the contract with plain S3 calls, with the gateway key on each host's prefix:
  - when a host is added;
  - before an enrollment is accepted;
  - in each host's daily check.

  A host whose storage fails cannot be added or enroll. The storage checks are in §4.5.
- **The floor is in storage, not in Holdfast** (decision 030). No Holdfast key can delete an object
  version for good or change the bucket's settings. So whatever is deleted can be recovered until the
  bucket's lifecycle rule expires its old versions. That holds against a compromised host, a compromised
  server and Prune now. The 7-day protection (§3.4) is Holdfast's own rule on top of it.
- **Repository passwords are escrowed in the bucket** (decision 036). `escrow.age_recipient` in
  `server.yaml` names an age public key whose private key is kept offline, in the recovery file (below). At host add,
  and whenever a host's password changes, the server writes `escrow/<agent>.age`: the agent's name, the repository URL and the
  password, encrypted to that key. A write that fails is shown on the host and tried
  again every 15 minutes; a host added before the recipient was set is written at the next pass. Without a
  recipient (a server set up by hand without one) nothing is written, and every host and `holdfast_escrow_configured`
  say so. There is no plaintext bundle of the passwords (decision 047): the escrow files and the recovery file open
  any repository without Holdfast.
- **The recovery file** (decision 047). Unless given an age public key of the admin's own
  (`HOLDFAST_ESCROW_RECIPIENT`), the installer makes the age key pair and writes `/root/holdfast-recovery.txt`, mode
  0600: the age private key (`AGE-SECRET-KEY-…`) and the server's credentials key (`HOLDFAST-CREDENTIALS-KEY-…`). With
  the bucket's two keys it rebuilds a lost server, and with the escrow files it opens every repository. It belongs
  offline: while a file is at `recovery_file` in `server.yaml`, the server shows the problem `recovery_file` (§5.5a).
  - `holdfast-server recovery-file -config server.yaml <path>` writes a running server's file: its credentials key,
    to which an admin with an age key of their own adds that key's `AGE-SECRET-KEY-…` line. `-recipient <path>`
    prints a file's age public key; `-new` is the installer's.
  - The credentials key never goes into the bucket. age encrypts but does not authenticate, so the maintenance key
    could write an export of its own to `meta/`; that the credentials key opens the export's passwords is what proves
    the export is the server's.
- **The nightly export** (decision 040), what a new server is restored from (§2.2,
  [runbooks/rebuild-holdfast.md](runbooks/rebuild-holdfast.md)). Every night at 00:30 in the default zone, the server
  writes `meta/<UTC date>.age` to its bucket with the maintenance key: every table of its database, the off-site
  instance's too, as CSV in a gzipped tar with a manifest, encrypted to `escrow.age_recipient`. It leaves out the
  sessions and the gateway's grants. The passwords inside stay sealed with the credentials key, so the export alone
  opens no repository. It also holds the server's files (decision 047): the agent listener's certificate and key, the
  CA and its key, the UI's certificate and key, and the settings in force without the database's DSN. The S3 keys,
  the database's password and the OIDC client secret are not in it: the admin gives them again.
  - A server that missed 00:30 exports at its next start; a failed export is tried again every hour.
  - With `offsite:` each export is written to the off-site bucket too, with the off-site key. A failed off-site write
    leaves the export counted, and is a problem and an alert of its own.
  - The newest 30 are kept in each bucket; older ones are deleted, and versioning keeps them for the lifecycle
    rule's days.
  - `holdfast export now` (an admin's) writes one at once, and `holdfast export list` shows the latest; the UI has an
    Exports page with both. Each export is audited (`export.written`, `export.failed`, `export.offsite_failed`).
  - Without an age recipient no export is written, and the server says so. `holdfast status`, the UI and
    `holdfast_export_last_success_timestamp_seconds` show the server's own problems: `export` and `export_offsite`.
  - `holdfast-server restore-export -identity <recovery file> (<file> | -latest)`, as root on a rebuilt server, loads
    one into a new, empty database, and refuses any other. It is not an API: there is no server yet (§3.3). It also:
    - writes the recovery file's credentials key where `server.yaml` says, and refuses another key that is there;
    - writes each of the export's files that is missing where the new `server.yaml` says, never over one that is
      there, and the exported settings beside it as `server.yaml.from-export`, to compare;
    - brings back every host that has an escrow file in the bucket but is not in the export, one added after it, as
      `holdfast agent import` does (§5.7): with its repository and password, no job, and waiting for a new enrollment
      code.

    An age key file alone works as the identity too, with the credentials key already in place. The installer's
    server line with `HOLDFAST_RECOVERY_FILE` does all of it and starts the server (§2.4).
  - After a restore, an agent that runs a revision newer than the restored head keeps it: the server sends it no
    config, and the host shows `ahead`, until a new revision changes the host.
- Credentials are never shown in the UI.
- There is no raw restic console.

### 2.4 Packages and upgrades

- Holdfast ships `.deb` and `.rpm` packages for amd64: `holdfast-agent`, `holdfast-server`,
  `holdfast-cli`, and `holdfast-restic`, which holds the pinned restic at
  `/usr/libexec/holdfast/restic` and which the agent and the server need at their own version. The binaries
  are static, so one package runs on every supported distribution. The packages are unsigned for now, with
  `SHA256SUMS`. [install.md](install.md) is the guide. `holdfast-freshness`, M1's storage witness, left
  in M10 (decision 045); `holdfast-server` replaces it.
- **The agent's container image** (decision 055), `ghcr.io/kabsdk/holdfast-agent:<release>`, such as `0.1-dev`, and
  `:<build>` for each build, is published by the release from `packaging/container/Dockerfile`: Debian, the agent, the
  pinned restic and PostgreSQL 18's client in `/usr/lib/postgresql/18/bin`. It runs as uid 65532 and needs no
  Kubernetes API access. It reads `/etc/holdfast/agent.yaml` (a ConfigMap) and keeps its key, certificate, cache,
  credentials and restores in `/var/lib/holdfast-agent` (a volume), so `agent.yaml` sets `state_dir`,
  `credentials_dir` and `restore.root` there. Its entrypoint is `holdfast-agent run --enroll-code-file
  /run/holdfast/enroll/code`: on its first start it enrolls with the code in that file (a Secret), without rewriting
  `agent.yaml`. One Deployment, with the `Recreate` strategy, per database server.
- Installing enables no service: each needs its config first. Upgrading restarts a service that runs.
  Removing stops and disables it, and never deletes `/etc/holdfast`, state or backups.
- **`install.sh`** (decision 044), released beside the packages, installs them and sets the machine up in one line run
  as root, by role: `server`, `agent` (the default) or `cli`, with `HOLDFAST_*` settings in the environment.
  It checks every setting before it changes anything and stops with the list of what is missing or wrong; it never
  prompts. Run again, it only upgrades.
  - The agent's line needs `HOLDFAST_SERVER`, `HOLDFAST_AGENT_NAME` and `HOLDFAST_CODE`, as `holdfast agent add`
    prints them (§5.7). The name is never read from the machine (decision 056).
  - The server's own name, for its URLs and certificates, is `hostname -f`.
  - The server's line needs the bucket and its two keys. It writes the recovery file unless given an age public key
    (§2.3), and turns the web UI on at `https://<agent>/` with a certificate from Holdfast's own CA, unless given one or
    `HOLDFAST_UI_URL=off` (decision 049). OIDC, alerts (`notify:`) and off-site copies are optional settings.
  - On a new machine, the server's line with `HOLDFAST_RECOVERY_FILE` and the settings of the first install rebuilds
    a lost server from the bucket's newest export, and starts it (§2.3). With `/etc/holdfast` put back instead, it
    prepares a rebuild and leaves the server stopped for `restore-export`.
- **The Ansible collection `kabsdk.holdfast`** (decision 050), released beside the packages, does what `install.sh`
  does with the same settings in lower case, as the roles `server`, `agent` and `cli`. It differs from the installer
  in what it keeps:
  - `server.yaml` follows the settings on every run.
  - The roles read no name from the machine: the server role requires `holdfast_fqdn`, the server's DNS name, and the
    agent role `holdfast_agent_name`, the agent's name (decision 056). They need no gathered facts.
  - The agent role adds the agent and enrolls it with the `holdfast` command as root on the server. It refuses a name
    that differs from the one the agent enrolled with, and writes a name given as `fqdn:` in `agent.yaml` as
    `agent_name:`.
  - It makes the jobs an agent lacks, each given as a mapping of `holdfast job set`'s flags, and never changes or
    removes one. A PostgreSQL job names a connection, which is set in Holdfast, not by the collection.
  - It sets what only root on the agent sets in `agent.yaml` when it is given: the restore limits, and the PostgreSQL
    client (`holdfast_postgresql_bin_dir`, `holdfast_postgresql_allow_in_place_restore`). It takes out a `dumps:` that
    0.5.0 left there (§4.6).

  A run with nothing to do changes nothing.
- **The agent and the server agree on a protocol version.** The agent sends its own with every poll (an agent
  before M5 sends none and speaks 1), and the server answers with the range it accepts. The server accepts
  every agent from its minimum on, so it is upgraded first, and agents after it, one by one. Protocol 2 (M6)
  added dump jobs (removed in 0.6.0, decision 052), restores from another host, and pause and resume; protocol 3 (M7)
  added restores from a vault's copy; protocol 4 (M8, decision 041) is the agent's own certificate in place of a token,
  backups through the gateway, and restores from off-site through it; protocol 5 (decision 055) added PostgreSQL jobs,
  connection tests and restores into a database. An agent before 4 cannot log in at all: it enrolls again after its
  upgrade. A feature an agent does not speak is refused for it, and the rest goes on: a PostgreSQL job on an agent
  before 5 is the error P004 (§4.5).
- An agent older than the server accepts is refused with 426 and the range. It logs that it must be
  upgraded, keeps backing up on its cached config, and the server still takes its reports. It gets no new
  config and no commands meanwhile. The host shows the `agent_too_old` problem, and
  `holdfast_agent_too_old` is 1.

---

## 3. Configuration model

### 3.1 Authority and drift

- Holdfast is the authority for config.
- Drift is detected and shown.

### 3.2 Revisions

- There is no Git and there are no approvers.
- Every save creates a revision in PostgreSQL. A revision holds its number, who made it, when, a message,
  and the full config: the agents, their PostgreSQL connections without passwords (§2.3), and their jobs.
- Any two revisions can be diffed.
- Each revision also stores the warning codes that were accepted for it (§4.5).

**There is deliberately no rollback of the config.** (Rolling a repository's files back is another thing:
§3.5b.) To undo a change, edit the config and save it. That save passes the
same checks as any other change. A rollback would be a bulk edit in disguise, and it could silently cut
retention. See [decision 013](decisions/013-no-rollback.md).

### 3.3 Interface parity and audit

- The options in the UI, the API and the CLI are the same, with very few exceptions. Any exception is listed
  here:
  - **Root's link into the UI**, `holdfast ui-login` (`POST /api/v1/ui-login`), exists only on the server's root
    socket, never over the network (§6.1, decision 049). M9's `holdfast escrow export`, the one before it, is gone
    (decision 047).
  - **Settings in `server.yaml`** are changed by editing the file as root. The UI and the API only show them,
    without secrets.
  - **Login** differs by client: a browser is redirected to the provider or opens root's link, the CLI shows a device
    code (decisions 034, 049). Both end in the same kind of session.
  - **Off-site copies** (§3.7) are the same UI, API and CLI on the off-site repositories: the pages under
    `#/offsite/`, the API under `/api/v1/offsite/`, the CLI with `--offsite`. UI = API = CLI holds there too.
  - **Restoring an export** is `holdfast-server restore-export`, as root on a rebuilt server, before it starts
: there is no server to ask yet. Writing and listing exports is UI, API and CLI as usual (§2.3).
  - **What only root on an agent sets** is in its `agent.yaml`, never in the UI, the API or the CLI, which only show
    it: the restore limits (§5.4) and the PostgreSQL client with `postgresql.allow_in_place_restore` (§4.6). A
    compromised server or admin login is then not root on every agent (decision 030).
- The API comes first. The UI and the CLI are thin clients of the HTTP API, and all logic, validation and
  checks live behind it (decision 028).
- Every action is recorded in the host's history and in the audit log.

### 3.4 Retention safety

These rules hold whatever the config says.

**High-water mark.** Maintenance never prunes a snapshot that any policy in force during the last 7 days
would keep.

**The daily run**. Once a day per host, the server:
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
- `holdfast retention run <agent>` starts the same run now, with the same protection.
  `maintenance.retention: off` in `server.yaml` turns the daily run off.
- Removing a job never deletes data (§3.5): the daily run covers the jobs in the current config only.

**Pending.** A retention cut becomes pending for 7 days, however the cut was made. `holdfast pending
<agent>` (`GET /api/v1/agents/{agent}/pending`) shows each waiting snapshot, the revision whose policy still
keeps it, and when the daily run may forget it.

**The hold** (decision 039), with off-site copies (§3.7). The daily run does not forget a snapshot that is not
copied off-site yet, while the snapshot is younger than 7 days; it is pending, marked "not copied off-site yet". What
is not copied is worked out from the primary snapshots of the jobs copied and the copies last reported, so the hold
holds while the off-site bucket is down or cannot be written. After 7 days the snapshot is forgotten as usual: a copier
that stalls is an incident, which its lag alert reports long before, not a reason to keep primary data for ever.
Prune now and Delete now are not held. Without off-site copies nothing is held.

**Keep longer.** `holdfast job keep-longer <agent> <job>` saves a new revision in which the job has an older
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
- The admin confirms by typing the host's short name (`holdfast prune-now <agent>` shows the page;
  `--confirm <short name>` goes ahead).
- It is recorded in the host's history and in the audit log.
- The deleted snapshot IDs can still be found in search, marked **Deleted**.

**Delete a snapshot** (decision 031)
- An admin can delete snapshots, with restic's `forget <id>`. It is a UI action on the job page's calendar (§5.2) and
  on the snapshot page, an API endpoint (`POST /api/v1/agents/{agent}/snapshots/{id}/delete`), and `holdfast snapshot
  delete <agent> <snapshot>...`, which takes several.
- **A deletion always says why** (decision 055): the UI asks for a reason, the API refuses one without a `message`,
  and the CLI one without `--reason` (`-m`). The reason goes into the audit log and the agent's history, and shows on
  the pending deletion.
- It becomes a pending deletion for 7 days, which can be cancelled (`--cancel`). **Delete now** (`--now
  --confirm <short name>`) skips the wait, with the same confirmation as Prune now.
- With off-site copies, the UI deletes in primary (site A), off-site (site B), or both. Deleting in primary never
  deletes the off-site copy (decision 041): the copy is marked **Deleted in primary**. Both is two deletions, one in
  each site, each pending and cancelled on its own. Off-site, the CLI is `holdfast --offsite snapshot delete <agent>
  <snapshot>... --reason …`, and the off-site pages' **Delete here too**; that deletion follows the same rules off-site.
- The snapshot page shows when the data is really gone in each bucket. Space is freed at the next prune, and
  S3 versioning keeps the old files until the bucket's lifecycle rule expires them. The daily storage test
  reads that rule's days (B011), so the date is the prune's date plus those days.

**Past prunes.** `holdfast prunes [<agent>]` lists each forget and prune: when, the daily run or Prune
now or Delete now, who, the snapshots forgotten, what restic said the prune freed, and when the data is
really gone. Snapshots Holdfast deleted keep tombstones marked **Deleted by Holdfast**; they never count as
disappeared (§3.6).

### 3.5 Removing and decommissioning

- Removing a job or a host never deletes data.
  - A removed job leaves the config. Its snapshots stay, and the daily run no longer touches them. A removed
    PostgreSQL job's dumps still restore into a database, as they did: the server takes the job, its connection and
    database, from the newest revision that had it, and the snapshot list carries them (`postgresql`).
  - A removed host is retired: its agent's certificate stops working, and its repository stays and can still be
    browsed. Adding the same name again brings it back, with its repository, after a new enrollment. Its agent was
    stopped on purpose, so `/metrics` drops it (the `holdfast_agent_*` series and its repository check's time), and
    `HoldfastAgentOffline` and `HoldfastMaintenanceStale` do not fire for it; its repository's metrics stay.
    `holdfast agent list` and `agent show` give its status as `retired`, with the date its repository is deleted
    while a decommission waits, and `decommissioned` after it, never `agent offline`.
  - A PostgreSQL connection is removed only when no job uses it, and its passwords go with it.
- Deleting data is a separate **decommission** action:
  - only for a removed (retired) host, only by an admin (`host.decommission`), who types the agent's name:
    `holdfast agent decommission <agent> --confirm <agent>`;
  - it waits 7 days, like every other deletion, and `--cancel` stops it meanwhile. A host waiting for it
    cannot be added back until it is cancelled;
  - then the server deletes every current object under `repos/<agent>/`, and the escrow file, with the
    maintenance key. S3 keeps their old versions until the lifecycle rule expires them, so the host's page
    gives the date the data is really gone: the decommission's date plus the rule's days (B011). Its
    snapshots keep tombstones marked **Deleted by Holdfast**;
  - a decommissioned host added again starts over, with a new repository and a new password.
- The repository's size is measured once a day: one listing of the prefix with its versions gives the
  current bytes and objects, and the bytes of the old versions S3 keeps. The host's page and the metrics
  `holdfast_repository_bytes{versions="current"|"noncurrent"}` and `holdfast_repository_objects` show it.
- Each job's **Stored** is measured with it: what the job's snapshots take in the repository, each piece of data once,
  as stored (`restic stats --mode raw-data --tag job=<job>`, run by the server). Jobs share data, so together they can
  take more than the repository. A job without snapshots has none, and one restic could not measure keeps its last.
  The job's `stored` in the API (`{bytes, measured_at}`) and `holdfast agent show` show it; off-site measures its own
  copies the same way. A host is measured once more as soon as its first backup is there.

### 3.5b Rolling a repository back

For a repository damaged or emptied behind Holdfast's back (decision 032), or after a mistaken Prune now
(decision 038):
- `holdfast agent rollback <agent> --to <time>` (`GET /api/v1/agents/{agent}/rollback?to=…`) shows what rolling the
  host's prefix back to that time changes: objects brought back and set aside, by kind, the snapshots that come
  back and those set aside, and when snapshot files were deleted. `--confirm <agent>` does it; admin only
  (`repository.rollback`). The UI's host page has the same, and the "disappeared" alert links to it.
- It copies each object's version of then over today's, and puts a delete marker in front of each object that was
  not there then. **No version is deleted**, so a rollback can itself be rolled back, to a time just before it.
  That makes it the third exception to the 7-day protection, beside Prune now and Delete now.
- `locks/` is left alone; `keys/` and `config` are rolled back with the rest.
- It is refused for a time before the host's last password rotation, or older than the lifecycle rule's days
  (B011), and when the maintenance key may not read old versions (`s3:GetObjectVersion`).
- The host's agent is paused and idle while it runs (`agent_down` when the host is down instead); then `restic
  check` runs, the snapshot list is read again, and the agent resumes and catches up on slots it missed.
  Snapshots set aside get tombstones marked deleted by Holdfast; snapshots that come back are no longer
  disappeared. `holdfast agent rollbacks <agent>` lists each rollback and how it went.
- A rollback past a prune brings back what the prune removed; the daily run forgets it again if no retention in
  force during the last 7 days keeps it.

### 3.5a A compromised host

`holdfast agent rotate <agent>` (`config.storage`) does, in this order:
1. adds a new restic password to the repository, and checks that it opens it;
2. stores it, unpins the agent's certificate (the host waits for a new enrollment, which makes it a new key) and
   issues a new enrollment code;
3. removes the old password's key from the repository. If that fails, the host shows the `rotation`
   error, and the next rotation removes it first;
4. writes the escrow file again.

The new password is stored before the old key goes, so the server always has a password that opens the
repository. The host has no S3 key to replace (decision 041). The runbook,
[runbooks/compromised-host.md](runbooks/compromised-host.md), says what else to do: rebuilding or cleaning the
host, and finding the last good snapshot.

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

### 3.7 Off-site copies (optional)

Decisions 027, 031, 039 and 041. **Off-site copies are optional.** Without an `offsite:` section in `server.yaml`,
Holdfast keeps one bucket: nothing is copied, no off-site page, setting, column or action is shown, no snapshot is held
back for a copy, and no off-site metric or alert exists. With it, holdfast-server copies every host's snapshots into a
second bucket, best in another site, with a key of its own there. Adding the section later starts copying from then on.

**How it runs.** The server runs a second instance of its own code beside itself, the **off-site instance**, on the
off-site bucket, with its tables in the schema `offsite` of the same database. The two talk in-process: every
`offsite.poll` (5 minutes by default) the off-site instance hands over its report and takes the feed: every enrolled
host, its jobs as they run, its PostgreSQL connections without their passwords, its repository's password, the
snapshots deleted on request, and the bindings. Nothing registers and there is no token.

**Hosts and jobs follow the server's**, by revisions of the off-site instance's own:
- a new host is copied at once, into its own off-site repository, with a password of its own, which the host never
  has; that password is escrowed in the off-site bucket (decision 036);
- a job's **off-site retention** is its primary retention, and follows every change there, until an admin saves one
  off-site: that one is the job's **own**, and stays (decision 053). **Match primary** makes it primary's again, and
  following again. Keeping less than primary is warning **O101**, which the admin accepts; a cut is R104 and waits 7
  days, as everywhere;
- a job's paths, or its engine and database for a PostgreSQL job, its schedule and its primary retention follow the
  server's, and so does its off-site retention unless it is its own; Exclude does not;
- **Exclude** stops copying a job, and keeps its copies under its off-site retention; **Include** copies it again;
- a job the server removes is **Retired**: its copies stay, under its off-site retention. A host all of whose jobs are
  retired can be removed off-site, and then decommissioned there, after 7 days;
- off-site, no host is added, no agent enrolls, nothing is backed up, no restore runs and no password is rotated.

**The copies.** For each host, the copier lists the primary repository, and copies the snapshots of the jobs it
copies that it has never copied: `restic --no-lock copy <ids>`. It reads the primary repository from a loopback
endpoint of the server that serves it read-only (the gateway's code, with a secret of its own), and writes the
off-site one through `s3:` with the off-site key, so restic needs only that key. The off-site repository is made once
with `init --copy-chunker-params`, so copies deduplicate. A copy keeps its host name, tags and time, and names its
source snapshot (`original`). Each copy is a run in the host's off-site history, so it counts as made by Holdfast. A
copy that the off-site retention or Delete here too removed is not made again. Once a day, after a copy, the
off-site repository is checked as an agent checks its own (1/`check_cycle_days` of the data), and its daily run
forgets and prunes behind the 7-day protection.

**Deleting.** Off-site copies are forgotten only by their own retention, after the 7-day protection, or by an
admin's Prune now or Delete now there. Deleting a snapshot in primary never deletes its copy: the copy is marked
**Deleted in primary** (decision 031), and an admin may **Delete here too**, which waits 7 days like any deletion, or
Delete now.

**Where people see it.** The same UI, API and CLI, with the same login and the same bindings:
- each host's page has an **Off-site copies** part: the copies, what is not copied yet, the last copy and its error,
  and each job's off-site retention, with "keeps less than primary";
- the off-site pages are the same pages under `#/offsite/`, in red, with the badge "OFF-SITE" and only the off-site
  actions; their API is `/api/v1/offsite/…`, the same routes, and their CLI is `holdfast --offsite …`, the same
  commands. `holdfast offsite exclude|include|match-primary` act on the off-site jobs, and `holdfast offsite copies
  <agent>` lists a host's copies;
- the off-site instance's config history and audit log are its own: `holdfast --offsite revision list` and
  `holdfast --offsite audit`.

**A restore from off-site** runs on the host, into a new or another directory, or a PostgreSQL dump into a new
database, never in place: `holdfast restore
<agent> <snapshot> --from-offsite`, with the copy's ID or its original's. The host's agent gets the copy's password in
the restore command, sealed, once, and reads its own copy through the gateway's off-site endpoint
(`/gateway/v1/offsite/<agent>/`), read-only, until it reports the restore. Outside such a restore a host cannot read
its off-site copy at all. The grant is in the off-site audit log.

**The hold** (§3.4) keeps, in primary, a snapshot not copied yet, for up to 7 days.

**The lag.** A job's newest copy is behind its newest snapshot in primary by `holdfast_offsite_behind_seconds`, on the
server's `/metrics`; `HoldfastOffsiteLag` pages past twice the job's longest gap. `HoldfastOffsiteCopyFailing` raises
a ticket when a host's copies keep failing. Off-site, the stale alert does not apply: the jobs are backed up in
primary.

**Storage off-site** passes the bucket's side of the storage contract (B004, B008–B012) with the off-site key.

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

**Run controls**:
- A scheduled backup that fails is retried after 5, 15 and 45 minutes, as long as the retry starts before
  the job's next run. Each retry is a run in the history, marked "retry 1 of 3" and so on. A run stopped at
  its timeout is not retried: it already took the whole gap.
- A job may cap its upload in KiB/s, which is restic's `--limit-upload`.
- The agent's service runs at low CPU and I/O priority (`Nice=10`, `IOSchedulingClass=idle`), so a backup
  gives way to the host's own work.

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

**The default job** (decision 048). `holdfast job set <agent> <job> --path …` (`PUT /api/v1/agents/{agent}/jobs/{job}`)
makes a job, and the request may leave things out:
- a new job takes what it does not give from the default job: daily at 02:00 with 20 minutes' spread; one per day
  for 14 days, per week for 8 weeks, per month for 12 months, per year for 3 years; the newest 3. A new job that
  names a schedule form takes the schedule as given, with no spread unless it gives one: the default's spread suits
  the default's form only, and over `--every 1m` it would fail S001;
- an existing job keeps its own values for what is not given. A schedule that names a form (daily, every, weekly)
  replaces the form and keeps the spread and zone unless it gives them; a spread or a zone alone changes only that. A
  retention, when given, is replaced whole. Giving paths to a dump job from before 0.6.0 makes it a paths job (§4.6).

So `holdfast job set web01 system --path /etc` is a whole job, and `holdfast job set web01 system --daily 03:00`
moves its time and keeps the rest; any `--keep-*` flag gives the whole retention. The checks (§4.5) run on the job as
merged.

**A PostgreSQL job** (§4.6) gives `--connection` and `--database`, or `--globals`, in place of `--path`. Without a job
ID, `holdfast job set <agent> --connection main --database app --database wiki --globals` makes a job for each
database, named after it, and `roles` for the globals, all with the same schedule and retention, in one revision
(`POST /api/v1/agents/{agent}/jobs`, with the jobs by ID; each is checked, and a finding names its job). A database
whose name is not a job ID (lower-case letters, digits and hyphens) needs one given: `holdfast job set <agent> my-db
--connection main --database My_DB`.

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
| P001 | The agent's pg_dump is older than the PostgreSQL server's major version, as the connection's last test found it. pg_dump must be at least as new as the server. |
| P004 | The agent cannot run PostgreSQL jobs: its agent is older than protocol 5 (§2.4), or it has no PostgreSQL client (`postgresql.bin_dir` in its `agent.yaml`, or no pg_dump that runs there). |

**Warnings** must be accepted one by one.

| Code | Condition |
| --- | --- |
| S002 | Runs are scheduled more often than they take (p95 run time). |
| R006 | Keep newest is 0. |
| R101 | The oldest restore point is less than 30 days old. |
| R102 | More than 1000 snapshots would be kept. |
| R103 | The longest gap between runs is more than 1 day. The message gives the worst-case data loss, spread included. |
| R104 | The change cuts retention. |
| P003 | The job's database was not on the server at the connection's last test. |
| P005 | The job's connection has never been tested, so it is not known to work, or its last test failed. |

The P checks are on PostgreSQL jobs only (decision 055). P002 is not used.

**Info**

| Code | Condition |
| --- | --- |
| I001 | There is no yearly tier. |

D001, D002 and I002 checked dump jobs (decision 037), which 0.6.0 removed (decision 052). D001 was a dump that
`agent.yaml` did not define, D002 an agent too old for dump jobs, and I002 a dump job on a host whose agent had not
enrolled. The codes are retired, and not used again.

**Storage checks** test the S3 server, not the config (decision 029, [storage.md](storage.md)). They are
errors that cannot be accepted. A host whose storage fails one cannot be added or enroll. Hosts hold no S3 key
(decision 041): B001, B002 and B005–B007 test the server's gateway key, on the host's prefix.

| Code | Condition |
| --- | --- |
| B001 | The gateway key cannot write under the host's prefix, or cannot delete under its `locks/`. |
| B002 | The gateway key cannot read back or list the host's prefix. |
| B003 | The maintenance key cannot read what the gateway key wrote. |
| B004 | The maintenance key cannot write, list or delete under `repos/`. |
| B005 | The gateway key can delete outside `locks/`. |
| B006 | The gateway key can delete a version for good. |
| B007 | The gateway key can use the bucket outside `repos/`, such as the escrow files and the nightly exports. |
| B008 | Bucket versioning is not enabled. |
| B009 | The maintenance key can delete an object version for good. |
| B010 | The maintenance key can change the bucket's settings: versioning, the lifecycle rule or the bucket policy. |
| B011 | No enabled lifecycle rule expires noncurrent versions under `repos/`. |
| B012 | The maintenance key cannot write, read back and delete under `escrow/`. Tested only when `escrow.age_recipient` is set (decisions 036, 047: `restore-export` reads the escrow files of hosts added after the export). |
| B016 | The maintenance key cannot write, read back and delete under `meta/`, where the nightly exports go. Tested only when `escrow.age_recipient` is set (decision 040). |

B014 and B015 tested the read-only keys of M6 and M7; no such key exists since decision 041, and the codes are not
used again. B013 was never used. The off-site bucket passes B004, B008–B012 and B016 with the off-site key.

- The accepted codes are stored in the revision and in the audit log.
- The CLI takes them explicitly, for example `holdfast job set … --accept R102,R104`. Without them it
  refuses the change and prints the same explanation as the UI. The API takes them as `accept`, and
  answers a refused save with 422 and every finding.
- `holdfast job check` (`POST /api/v1/agents/{agent}/jobs/{job}/check`) runs the checks without saving.
- S002 needs at least 5 finished runs. R101, R102 and R103 are estimates from the schedule and the
  windows, so their messages say "about".
- restic decides what each policy keeps (decision 030). Holdfast runs `restic forget --dry-run` for the
  old and the new policy. A change is a retention cut (R104) when the new policy keeps a strict subset of
  what the old one keeps. Removing a tier is therefore not a cut if a finer tier keeps the same snapshots.
  A job with no snapshots yet has nothing to cut. If restic cannot be asked, the change counts as a cut.

### 4.6 Databases: PostgreSQL jobs, and exports

A database's files are not a backup while it runs (§1.2). A PostgreSQL database is backed up by a PostgreSQL job,
which dumps it with pg_dump into restic (decision 055). Any other database, a directory server or anything else like
them is backed up from an export the host writes itself (below).

**Connections.** A connection is how an agent reaches a PostgreSQL server, and belongs to that agent. It is set
centrally, in the UI on the agent's page, with `holdfast agent connection set <agent> <name> --url <url> --user <user>
(--password-file <file> | --no-password) [--restore-user <user> --restore-password-file <file>] [-m <message>]`, or
`PUT /api/v1/agents/{agent}/postgresql/{name}`:
- the name: lower-case letters, digits and hyphens, at most 63, such as `main`;
- the URL: libpq's connection URI, `postgresql://`, with a host, such as
  `postgresql://db01.example.internal:5432/?sslmode=verify-full&sslrootcert=/etc/holdfast/pg/ca.crt`, or a socket
  directory, `postgresql:///?host=/var/run/postgresql`. It holds no user, password, `passfile` or database: they
  have fields of their own, and each job names its database. Paths in it are on the agent;
- the user that backs up, a plain role name. It needs `pg_read_all_data`, and `BYPASSRLS` where a table has row-level
  security;
- its password, read by the CLI from a file or standard input (`-`), never from its command line. A password not given
  keeps the one stored; `--no-password` removes it, for peer authentication on a socket;
- optionally, a restore user and its password. It needs `CREATEDB` and membership of the roles that own the databases,
  so that a restored database belongs to its owner. Without it, a dump restores as a file only.

Setting a connection saves a revision, its passwords sealed apart from it (§2.3), and needs `config.storage`.
Removing one (`holdfast agent connection remove <agent> <name>`, `DELETE …`) is refused while a job uses it, and
removes its passwords. `holdfast agent connection list <agent>` (`GET /api/v1/agents/{agent}/postgresql`) shows each
connection, whether its passwords are set, the jobs that use it, and its last test.

**Tests.** Saving a connection makes the agent test it at once; `holdfast agent connection test <agent> <name>`
(`POST …/postgresql/{name}/test`, `config.jobs`) tests it again, and the CLI waits for the answer (`--wait`, a minute
by default). The agent connects as the backup user, as a backup would, and reports the server's version, TLS, its own
pg_dump's version, and every database that takes connections, with its size and owner. With a restore user it also
connects as that user, and reports whether it may create databases and which roles it is a member of. The job form
offers the databases of the last test. Each test is in the agent's history and the audit log.

**The agent's part** is in its `agent.yaml`, which only root on the agent sets:

```yaml
agent_name: db01
server: https://holdfast.example.internal:8443
ca_sha256: …
postgresql:
  bin_dir: /usr/lib/postgresql/18/bin   # pg_dump, pg_dumpall, pg_restore and psql
  allow_in_place_restore: false         # true lets an admin restore a dump over the database it came from
```

Without `bin_dir` the agent takes no PostgreSQL job (P004). It reports its pg_dump's version and whether restores in
place are allowed.

**Jobs.** A PostgreSQL job names a connection of its agent and one database (letters, digits, `_`, `-` and `.`, at
most 63), or the globals; it has no paths. A job whose connection the agent does not have is refused, and so is a
second job of one agent that backs up the same. One job a database, each with its own schedule and retention;
`holdfast job set` makes several at once (§4.3).

**A backup** is restic's, with the dump as its one file:
`restic backup --tag job=<id> --stdin-from-command --stdin-filename <database>.dump -- <pg_dump>`, where `<pg_dump>`
is `env -i` and a clean environment (no `RESTIC_*` or `AWS_*`; the password in a pgpass file only the agent reads),
then `<bin_dir>/pg_dump --format=custom --compress=0 --lock-wait-timeout=10min --no-password --username=<user>
--dbname=<the URL, with the database>`. The globals are `pg_dumpall --globals-only --no-role-passwords`, into
`globals.sql`. The dump is uncompressed so that restic compresses and deduplicates it. A pg_dump that fails, or waits
more than 10 minutes for a lock, makes no snapshot, and the run fails as any backup does, with pg_dump's message.
Everything else is a job's as usual: the schedule, retries, retention with its 7-day protection, the storage witness,
off-site copies.

**Problems.** The agent shows `postgresql` while a connection's last test failed, and P001 while a connection's server
is newer than the agent's pg_dump (§5.5a).

**In Kubernetes**, the agent's container image (§2.4) backs up a database server from a pod, as one agent named after
the server, such as `k3s-postgres-main`. Its connection reaches the server's service, such as
`postgresql://postgres-main-rw.postgres.svc:5432/?sslmode=require`. With CloudNativePG, the cluster's
`managed.roles` makes the users: `holdfast_backup` with `inRoles: [pg_read_all_data]`, and `holdfast_restore` with
`createdb: true` and `inRoles` the owners of the databases.

**Exports, for other databases.** Holdfast runs no command a person defines on a host (decision 052): no dump command,
and no hook before or after a backup. Anything else whose files are not a backup while it runs is backed up from an
export the host writes itself:
- a systemd service and timer on the host make the export, with a margin before the job's time (`OnCalendar=`, and
  `TimeoutStartSec=`, `SuccessExitStatus=` and `OnFailure=` as needed);
- the export writes to a temporary name, and is renamed only when it succeeded, so a running or failed export never
  replaces the last good file;
- a paths job backs up the export's directory;
- the host's own monitoring watches the unit. If an export fails, the job backs up the previous good file again, which
  costs nothing as restic deduplicates it, and the failed unit is what alerts.

Holdfast does not order the job after the export, and does not check the file's age; that is deliberate (decision 052).
The documentation site's *Other databases and exports* has an example, for MariaDB.

**Dump jobs from before 0.6.0.** Until 0.6.0 a dump job backed up what a command in the host's `agent.yaml` printed
(decision 037). What is left of them:
- `holdfast job set` has no `--dump`, the UI's job form has paths or a PostgreSQL database, and the API refuses a job
  given a `dump`, citing decision 052.
- A dump job left in the config is refused on the next save, with a message that says to give it paths. Every save
  checks the whole config, so until no dump job is left, any save is refused so. Giving it paths (`holdfast job set
  <agent> <job> --path …`) makes it a paths job; removing it is the other way. An agent of 0.6.0 fails its runs with the
  same reason, so it shows as a problem.
- An `agent.yaml` that still has `dumps:` loads: the agent logs that it is ignored, and the host's other jobs keep
  running. Agents no longer report dump names.
- A snapshot a dump job made is tagged `dump=<name>` beside `job=<id>`, and is restored into a new directory only
  (§5.4).

---

## 5. Restore and inspection

Restore and inspection behave like restic does on the host.

### 5.1 Navigation

- Path: Overview → Agent → Job → Snapshot page. The list of agents is **Agents** (decision 056).
- Every snapshot ID anywhere in the product is a link.
- A page shows its main actions; the rarer ones, such as Prune now, Roll back or Remove, are in its ⋯ menu.

### 5.1a Agent page

- Its name, and the address it last connected from ("from 10.0.4.17").
- One row per job: its state, its **last 14 days**, its last and next backup, what it takes in each site (**Stored**,
  §3.5), and **Back up now** and **Restore**
  (from the newest snapshot). A PostgreSQL job shows its database and connection. Paths, schedule and retention are
  on the job's page.
- **PostgreSQL connections**, when the agent has any or has a PostgreSQL client: each with its URL, its users,
  whether their passwords are set, the jobs that use it, and its last test: the server's version, TLS, pg_dump's
  version and the databases with their sizes and owners, or why it failed. **Test** tests it again; an admin can add,
  change and remove connections (§4.6). Passwords are typed into the form and never shown back.
- The last 14 days are a square a day, coloured by the worst backup of the day: failed, incomplete, backed up, or no
  run; a day before the job's first backup is drawn apart, as it had nothing to miss. The server puts each backup on
  its day, in the time zone asked for: `GET /api/v1/agents/{agent}/days?days=14&tz=…`
  and `holdfast agent days <agent>` (up to 92 days, in this machine's zone by default).
- Recent runs fold: a job's backups on one day that ended the same way are one line.

### 5.2 Job page

- **One calendar of the job's restore points**, a square a day: one calendar year in one row, January to December,
  each month's weeks as columns from its 1st, Monday to Sunday down the side. The year is chosen above it, each year
  with its number of restore points, from the oldest restore point's year to this one; it opens on the newest restore
  point's. The squares size themselves to fill the box, from 10 to 18 px. Every day shows: a restore point's state; no
  restore point kept (grey); before the oldest restore point (pale, outlined); still to come (a dashed outline, with
  only its date).
- With off-site copies (§3.7), a square's top half is primary (site A) and its bottom half off-site (site B), each in
  its own state:
  - primary: **kept**, **deletion waits** (a retention cut or a deletion in its 7 days, §3.4), or **deleted**;
  - off-site: **copied**, **copy pending**, **deletion waits**, **deleted**, or **never copied** (an excluded job, or
    a day before copying began).

  Without off-site copies a square is primary's alone. A day with no restore point is empty.
- Every square says on hover, or when the keyboard reaches it, its day and its restore points in each site.
- **Choosing a day** lists its restore points right under the calendar, which flashes so each click shows its answer:
  each with its time, its snapshot (a link), restic's reasons for keeping it (such as "daily within 14d"), its state
  in each site, and **Restore** and **Delete**.
- **Several days** are chosen with shift-click, or a whole month by its name; Delete then deletes all their restore
  points, with one reason (§3.4).
- The job's retention rules are chips; choosing one highlights the days it keeps.
- The card's header has a line per site: primary, and off-site when there is one, each with what the job takes there
  (**Stored**, §3.5).
- A PostgreSQL job's page names its connection and database; each restore point is a dump.
- **Find a file in all snapshots**, one line at the top of the page, above the restore points, uses `restic find` to
  show the versions of a file. Each version links to the snapshot, opened at that folder.
- 0.7's squares per retention rule, its list of snapshots and its separate calendar page are gone:
  `#/hosts/{agent}/jobs/{job}/snapshots` opens the job page.

### 5.3 Snapshot page

| Tab | Behaviour |
| --- | --- |
| Files | One tree of the snapshot, names and metadata only: folders open in place, each entry with its change, mode, owner, size and time. A folder is read when it is opened (`restic ls` of that folder, 1 000 entries at a time). The change is `restic diff --metadata`'s mark against the snapshot before, or any older one chosen: **+** new, **−** removed (shown struck through, from the older snapshot), **M** content changed, **U** metadata only, **T** type changed, with their counts; a closed folder says how many changed inside. **Only changes** shows just those and their folders. **Find** looks for a name in this snapshot (`restic find --snapshot`). Each entry has **Restore…** (only that, §5.4), and a changed file **Diff…**: restore both versions to diff on the host (§5.4) and run the `diff -u` it prints, on the host. It never shows contents or line diffs, as restic does not. The first 10 000 changes mark the tree; `holdfast snapshot changes` lists all. `#/…/snapshots/{id}/changes` opens Files. |
| Restore | See §5.4. |

A PostgreSQL job's snapshot holds one file, the dump: it has only Restore.

Deleting the snapshot is the page's **Delete…** button, a dialog: in primary (site A), off-site (site B) or both, with
off-site copies; Delete in 7 days, or an admin's Delete now; and always a reason (§3.4). It is on no tab.

### 5.4 Restore

The Restore tab is one column: where from (with an off-site copy), what, where to, then the Run bar.

**Selection**
- A tree of the snapshot's paths, a folder opened at a time, with a tick each. Everything is ticked by default:
  what you untick appears as **Left out** chips (restic's `--exclude`). Unticking everything and ticking what you want
  makes them **Only** chips (`--include`). restic takes one or the other, never both, so a tick that would need both
  is greyed out.

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

**If a file already exists**, in place or in another directory, the choice is restic's own `--overwrite`:
`always`, `if-changed`, `if-newer` or `never`. The default is restic's.

**Running it**
- A **dry run** is available. It reports what would be restored, and writes nothing.
- A reason can be given; it goes into the audit log.
- The request gives either what was left out (`--exclude`) or only what to restore (`--include`, for
  **Restore only this**), not both. restic always runs with `--sparse --verify`.
- The restore runs on the host, through its agent, in the agent's one-restic-at-a-time loop. Its result,
  with counts and the target directory, is in the host's history and in the Restores tab (§5.5).
- A restore that the host has not picked up within 24 hours is cancelled, and the cancellation is reported.

**A PostgreSQL dump** (decision 055) restores onto its own database server only, in one of three ways:
- **Into a new database** beside the original (`holdfast restore <agent> <snapshot> --to-database`), named `<database>-restore-<YYYYMMDD>-<HHMM>` from the dump's time in the server's zone, cut to 63
  bytes. The snapshot list gives each such snapshot that name beforehand (`restore_database`), so every client shows
  the name the restore uses. It needs `restore.new_dir`, as a new directory does: it writes nothing that exists. The
  restore user creates it, owned by the original database's owner, and loads the dump into it (below). When the
  original database is gone, its roles may be gone too: the copy and everything in it belong to the restore user,
  without the dump's owners and grants (`--no-owner --no-acl`). If the load fails, the new database is left as it is,
  for an admin to drop.
- **In place**, over the database it came from: an admin's (`restore.in_place`), who types the database's name
  (`--in-place --confirm <database>`), and only where the agent's `agent.yaml` has
  `postgresql.allow_in_place_restore: true`. The agent first dumps the database as it is, into a snapshot of the job
  tagged `pre-restore=<restore>`; if that fails, nothing is restored. Then it loads the dump with `--clean --if-exists`:
  if anything fails, nothing changed. What the dump holds is replaced; tables made after the dump stay. A database
  that is gone is made again.
- **As the dump file**, into a new directory (the default) or another one, as any file is restored. The
  roles and globals restore this way only: Holdfast never creates roles.

Loading a dump streams it from restic through `pg_restore`, which writes it as SQL, into `psql --single-transaction`
with `ON_ERROR_STOP`, as the restore user: one transaction, stopping at the first error, nothing written to disk.
`psql` commits only once `pg_restore` has finished well. The agent's client is often newer than the server (one
client for every server it backs up, P001), and pg_restore's own session settings may be ones an older server does
not know: the agent leaves those out, such as `transaction_timeout` on a server older than PostgreSQL 17. A dry run
reads the dump with `pg_restore --list` and writes nothing.

The dump streams from restic into pg_restore, through no file on disk, and pg_restore waits at most 5 minutes for a
lock. A restore never names another database, never runs on another agent (`--to-host`), and from an off-site copy is
never in place. Without a restore user on the connection, a dump restores as a file only. A dry run reads the dump with
`pg_restore --list`, and writes nothing.

**A dump snapshot**, made by a dump job before 0.6.0 (§4.6), is one file: it is restored into a new directory only.
Loading it is the admin's job, on the host.

**SELinux labels** are restored as the snapshot has them, except for a dump snapshot's file and a restore from another
host: those keep the label the host gives a new file (`restic restore --exclude-xattr security.selinux`). A dump
has no label in its snapshot, another host's labels belong to its own policy, and SELinux refuses to remove a
label, which made restic fail (found in the lab, on el9 and ol9).

**Onto another host**: an admin (`restore.in_place` on both hosts) restores host A's snapshot onto
host B with `holdfast restore A <snapshot> --to-host B`:
- B must allow it in its own `agent.yaml` (`restore.allow_other_hosts`), and its agent must speak protocol 2;
- the target is a new or another directory on B, never in place;
- B's agent gets A's password in the restore command, which the server keeps sealed and clears once B has it; B
  keeps it in a root-only file for that one restore. B's root learns A's password;
- the gateway lets B's certificate read A's repository, and only read it, from the request until B reports the
  restore, at most 49 hours (decision 041); B runs restic without locks;
- A's history and B's both have it.

**From its off-site copy** (§3.7): `holdfast restore <agent> <snapshot> --from-offsite`, with the copy's ID or its
original's (`holdfast offsite copies <agent>` lists them), or **Restore from: Off-site copy** on the snapshot's Restore
tab:
- it runs on the snapshot's own host, into a new or another directory, never in place;
- it is granted at once, if the copy is still off-site; the host's agent gets the copy's password sealed, once, and
  reads the copy through the gateway's off-site endpoint, read-only and without locks, until it reports the restore.
  It needs no `allow_other_hosts`: it is its own data;
- a refusal is an error at once; the grant or the refusal is in the off-site audit log.

**Per-file actions**
- **Restore only this**.
- **Restore both versions to diff on the host** restores both versions into a directory only root can read,
  and prints the `diff -u` command to compare them. It is one API call (`POST …/restore-pair`) and one
  command (`holdfast restore pair`); the command is given once both restores are done.

### 5.5 Restores tab

- Shows the restore history for the whole fleet: who restored what, where to, the result, and a link to the
  snapshot.
- Has an agent → job picker that leads to that job's page.
- A restore into a database shows the database it went into, on which connection.

### 5.5a Problems

**The Overview**, the UI's start page, lists every host, those with problems first, and under each host what the
server knows is wrong with it, each problem once, with its code. Its tiles count the stale, failed and incomplete jobs,
with an off-site bucket the hosts whose copies fail or lag (`offsite_copy`, `offsite_lag`), and the hosts with any
other problem; choosing a tile filters the list by it. `#/problems`, the page it replaced,
opens it. The problems are:
- errors: `stale`, `failed`, `not_in_bucket` (the agent reported a backup whose snapshot the server, reading the
  repository from the bucket after the report, did not find: decision 045), `offsite_copy` (the last copy to the
  off-site bucket failed) and `offsite_lag` (a job's newest copy is behind by more than twice its longest gap), with
  an off-site bucket only, `storage`, `disappeared`, `foreign_keys`,
  `rotation` (an old password still opens the repository), and `decommission` when a due decommission could not
  delete;
- warnings: `incomplete`, `offline`, `drift`, `restic`, `not_made_by_holdfast`, `catalogue`, `retention`,
  `removing`, `enroll` (waiting to enroll again), `escrow` (no recipient, or the write failed), `size`,
  `agent_too_old` (§2.4), `decommission` while one waits, `rollback` while one runs (§3.5b), `ahead` (the
  agent runs a change a restored server lost, §2.3), `postgresql` (a PostgreSQL connection's last test failed, §4.6)
  and `P001` (a connection's server is newer than the agent's pg_dump, so no job can use it).

The server's own problems, which belong to no host, are on every page of the UI and in `holdfast status`: `export`
(no recipient, a failed export, or none for 2 days), `export_offsite` (§2.3), `recovery_file` (the recovery file is
still on the server, §2.3) and `notify` (an alert or the heartbeat could not be sent).

**Alerts** (decision 046). With `notify:` in `server.yaml`, the server sends these problems itself, errors and
warnings, by mail through an SMTP relay, to a webhook (a JSON POST whose `text` Slack and Mattermost show), or both:
- once when a problem has lasted `notify.after` (10 minutes by default), and once when it ends. A problem that ends
  sooner is never sent. A send that fails is the `notify` problem, and is tried again at the next pass, a minute later;
- after each pass that sent what it had to, it calls `notify.heartbeat_url` (GET), so a heartbeat service outside the
  site notices when the server, or its alerts, stop;
- `holdfast notify test` (`POST /api/v1/notify/test`, and a button on the UI's Settings page), an admin's, sends a
  test by every channel that is set, and is audited (`notify.test`).

Prometheus and the rules that ship with Holdfast are optional, for teams that have them ([alerting.md](alerting.md)).

### 5.6 Search

Search finds:
- agents;
- jobs;
- snapshot IDs, including deleted ones, which are marked **Deleted**.

### 5.7 Add agent

1. Enter the agent's name (decision 056): `holdfast agent add <agent>`, `POST /api/v1/agents` with the name alone
   (its field is `fqdn`). It is validated (§1.1), and a duplicate name is refused. No S3 key: the agent's storage is
   tested with the gateway key (decision 041, [storage.md](storage.md)).
2. Saving creates a revision. The agent has no job: jobs are added with `holdfast job set` (decision 048, §4.3). The
   UI's form asks for the paths of a first job too, and saves it with that second call, with the default job's
   schedule and retention. A PostgreSQL job comes after the agent has enrolled and has a connection (§4.6).
4. Holdfast shows a single-use enrollment code, valid for 15 minutes, and the one line to run on the machine as root
   (decision 044), `install_script`: `curl -sfL <release>/install.sh | HOLDFAST_SERVER=<url> HOLDFAST_AGENT_NAME=<agent>
   HOLDFAST_CODE=<code> HOLDFAST_CA_SHA256=<pin> HOLDFAST_VERSION=<the server's release> sh -`, which installs the
   agent's packages, checked against the release's `SHA256SUMS`, and enrolls. The server's release is its version, or,
   for a build between releases such as `0.1.0~dev90`, the development release of the next one, `0.1-dev`, whose
   newest build the line installs (decision 054). With `download_url` in `server.yaml`
   the line fetches from that mirror instead. For a host whose packages are installed already, `install_command` is
   the enrollment alone: `holdfast-agent enroll --server <url> --name <agent> --code <code> --ca-sha256 <pin>`. An
   agent in a container enrolls with the code alone, in the file its `--enroll-code-file` names (§2.4).
5. Enrolling makes the agent an ECDSA key and a self-signed certificate on the host, in a root-only file; the
   key never leaves the host. The agent pins the server (`--ca-sha256`), and the server pins the certificate the
   enrollment came with: from then on that certificate is the host, on every connection (mutual TLS). A new
   code and a new enrollment replace it; removing or rotating the host unpins it.
6. Progress is shown: **waiting → agent connected → first backup done**.

**Import a host from its escrow file**, beside Add host, for an admin on every host:
`holdfast agent import <agent> --escrow-file <file>`, or `POST /api/v1/agents/import`. It is for a host a rebuilt
server does not know, as the export it was restored from was made before the host was added. The decrypted escrow
file goes to the server once and is never shown back. The server checks that the file is that host's and names this
server's repository for it, tests the storage contract, and proves the password opens the repository; it then adds the
host with that repository and password, its keys as Holdfast's own, no job, and an enrollment code. It refuses a host
it knows: `holdfast agent enroll-code` is for that. `restore-export` does the same by itself for every host whose
escrow file is in the bucket (§2.3), so an import is needed only for a file from elsewhere.

---

## 6. Access control

### 6.1 Login

- **Through OIDC**, optional (decision 034): Authelia, Dex, Keycloak or any provider with the code flow and the
  device flow. The server is the only OIDC client:
  - a browser logs in with the code flow and gets a session cookie. A page opened without a session goes straight to
    the login, showing nothing but “Loading Holdfast…” on the way;
  - `holdfast login --server <url>` shows a device code to enter at the provider, and gets a session token.

  Groups come from the groups claim, read at login. An LDAP or Active Directory goes behind the provider; Holdfast
  has no LDAP of its own. `auth.oidc` takes the issuer, the client ID and the client secret, all three or none.
- **Root's link** (decision 049): `holdfast ui-login`, as root on the server, prints a link that works once, within
  5 minutes, and opens a browser session as `local:root`, admin on every host. It is the UI's login on a server with
  no provider, and the way in when the provider is down. Each link is audited (`auth.link_issued`), with the sudo user
  who asked for it. Links live in the server's memory, so a restart voids them. It needs `ui.listen` and
  `ui.public_url`.
- There are no local passwords. Break glass is root on the server: the root-only socket for the CLI, where root is
  admin on every host, and `holdfast ui-login` for the browser. Without a provider, the CLI is used there.
- A browser session lasts 12 hours and ends after an hour without use; a CLI session lasts 7 days. An admin
  can end a user's sessions (`holdfast access end-sessions <user>`).
- People use the UI and the API on their own listener, `ui.listen`. The installer gives it a certificate from
  Holdfast's own CA, which browsers warn about until they trust `/etc/holdfast/tls/ca.crt`, unless it is given one
  browsers trust. Agents use the agent listener.
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
| `host.decommission`: delete a removed host's repository, after 7 days (§3.5) | | | ✓ |
| `repository.rollback`: roll a repository back from S3's versions (§3.5b) | | | ✓ |

A user can have several bindings. They combine per host. **A host you have no binding for does not exist
for you:** it is not listed, not searchable and not counted (decision 033). Asking for it answers "not
found". Revisions and the audit log are cut down to your hosts. Nobody sees file contents.

Actions the table does not name:
- adding an agent, issuing an enrollment code and rotating its password
  (`holdfast agent rotate`) need `config.storage`;
- setting or removing a PostgreSQL connection needs `config.storage`, as it holds credentials; testing one needs
  `config.jobs` (§4.6);
- a restore into another directory needs `restore.in_place`, and a restore onto another host needs it on both;
- a dump restored into a new database needs `restore.new_dir`, and over its database `restore.in_place` (§5.4);
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

The off-site pages (§3.7) use the same bindings: the off-site instance takes them from the server at every sync, and
refuses to set its own.

### 6.4 `/etc/holdfast/server.yaml`

Editing this file needs root on the server. Holdfast reloads it, and the reload is audited.

```yaml
tls:                     # the agent listener's; the export carries these files (decision 047)
  cert_file: /etc/holdfast/tls/server.crt
  key_file: /etc/holdfast/tls/server.key
  ca_file: /etc/holdfast/tls/ca.crt        # Holdfast's own CA, when the installer made it; agents pin it
  ca_key_file: /etc/holdfast/tls/ca.key
recovery_file: /root/holdfast-recovery.txt   # decision 047: a problem while the file is here; it belongs offline
ui:                      # on unless the installer was given HOLDFAST_UI_URL=off (decision 049)
  listen: ":443"
  public_url: https://holdfast.example.internal
  tls: {cert_file: /etc/holdfast/tls/ui.crt, key_file: /etc/holdfast/tls/ui.key}
auth:                    # optional: without it, root logs in with holdfast ui-login
  oidc:
    issuer: https://auth.example.internal
    client_id: holdfast
    client_secret_file: /etc/holdfast/oidc-client-secret
    groups_claim: groups
  bootstrap_admins: []   # usernames that are admin everywhere at every login while listed, for the first login; each use is audited
escrow:
  age_recipient: age1…   # decision 036: an age public key; its private key is in the recovery file, offline
storage:                 # decision 041: two keys for the bucket; hosts hold none
  maintenance_credentials_file: /etc/holdfast/maintenance.env
  gateway_credentials_file: /etc/holdfast/gateway.env
notify:                  # optional (decision 046): the server's own alerts
  mail: {smtp: "relay.example.internal:587", tls: starttls, from: holdfast@example.internal, to: [backup-team@example.internal]}
  webhook_url: https://chat.example.internal/hooks/…
  heartbeat_url: https://hc.example.org/ping/…
  after: 10m
offsite:                 # optional (§3.7): a second bucket, and its key
  endpoint: https://rgw.site-b.example.internal
  bucket: holdfast-offsite
  maintenance_credentials_file: /etc/holdfast/offsite.env
download_url: https://mirror.example.internal/holdfast/1.0.0   # optional (decision 044): where hosts fetch install.sh
```

The full example is `deploy/examples/server.yaml`. `install.sh server` writes a first `server.yaml` (decision 044).

### 6.5 File contents

No file contents reach the UI, the API or the CLI. Only names and metadata do (§5.3). `content_view` was cut
(decision 030). A restore runs on an agent and writes to that host.
