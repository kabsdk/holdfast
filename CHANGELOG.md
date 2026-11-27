# Changelog

What each release of Holdfast changes. The Ansible collection has its own notes in
[ansible/CHANGELOG.md](ansible/CHANGELOG.md).

## 0.1-dev

The first release, as a development release: **it is subject to change.** Until 0.1.0 is released, each new build
replaces it under the same name and tag, `v0.1-dev`, and installs as an upgrade of the one before (decision 054).

- **holdfast-server**: config revisions in PostgreSQL, enrollment, the HTTP API, the web UI, the gateway through which
  hosts back up without an S3 key, maintenance under the 7-day protection, the storage witness, alerts by mail,
  webhook and heartbeat, Prometheus metrics, and the nightly export. A removed host's agent, stopped on purpose,
  is left out of the metrics, so it raises no `HoldfastAgentOffline` or `HoldfastMaintenanceStale`, and `holdfast
  agent list` shows it as retired, not offline.
- **holdfast-agent**: runs restic on each host on its own schedule, through the server's gateway; restores on the
  host; and keeps its events until the server is back. Its daily check says which part of the data it read. Holdfast
  runs no command a person defines on a host: an export of anything but PostgreSQL is the host's own job, on its own
  timer, and a Holdfast job backs up its files (decision 052).
- **Agents are named in their own config** (decision 056): `agent_name` in `agent.yaml`, set with
  `holdfast-agent enroll --name`, the installer's `HOLDFAST_AGENT_NAME` or the collection's `holdfast_agent_name`,
  and never read from the machine. The list is **Agents**, the CLI `holdfast agent …` (which replaces `holdfast host
  …`, with no alias) and the API `/api/v1/agents/…`. An agent shows the address it connects from. An `agent.yaml`
  with `fqdn:` still loads, with a warning.
- **PostgreSQL databases** (decision 055): a job of the engine `postgresql` dumps one database with `pg_dump`, or
  the roles with `pg_dumpall --globals-only`, straight into restic. Connections are set in Holdfast (`holdfast agent
  connection set|list|test|remove`), their passwords sealed and sent only to the agent, and tested by the agent at
  once. `holdfast job set <agent> --connection c --database a --database b` makes a job each. A dump restores into
  a new database beside the original, in place by an admin where the agent allows it (after a safety dump, in one
  transaction), or as a file; a dump whose database and roles are gone restores into a new database owned by the
  restore user, and so does one whose job was removed since. A dump loads into a server older than the agent's client
  too, such as PostgreSQL 16 with PostgreSQL 18's client. The checks P001, P003, P004 and P005, and the problem `postgresql`. Agent protocol 5.
- **The agent's container image**, `ghcr.io/kabsdk/holdfast-agent`, with restic and PostgreSQL 18's client, backs up
  a PostgreSQL server in Kubernetes from a pod.
- **holdfast**: the CLI, with every action the web UI has.
- **The web UI**: an **Overview** of every agent, worst first, whose tiles count stale, failed and incomplete jobs and
  filter the agents by them; an **agent page** with a row per job, what it backs up (a path longer than 50 characters
  cut in its middle) and its last 7 backups as squares, a backup outside the schedule filling the slot it ran in, and
  its PostgreSQL connections, and what each job takes in each site (**Stored**, from `restic stats --mode raw-data`,
  measured after each new snapshot), with one subject per line and a site named only where there are two; a **job
  page**, its paths and exclusions folded into one line when there are many, that is one **calendar** of the job's
  restore points, a square a day, a year in one row with the year chosen above it, primary and off-site in its two
  halves, where a day opens its restore points with Restore and Delete, their **Size on host** and **Added to S3**
  (restic's `data_added_packed`; the API and the CLI sort by it, `--sort added`), and **Find a file** at its top, with
  restic find's Ignore case and modified days; a snapshot's **Files** as one tree with what changed against an older
  snapshot (`holdfast find --snapshot` finds a name in one snapshot); and a snapshot's **Restore** as a tree of ticks,
  restic's `--exclude` or `--include`, in one column. Lists share their columns, evenly spaced, nothing running into
  the next; the breadcrumbs follow the side menu, every part a link; each retention tier is in its own unit.
- Retention by time windows, decided by restic's `forget --dry-run`; checks with stable codes; deleting snapshots,
  always with a reason, in primary, off-site or both; Prune now; rolling a repository back from S3 versions, picking
  what was deleted or overwritten to bring back, while the gateway holds the agent's writes; restoring onto another
  host.
- **Stale restic locks clear themselves** (decision 059). When restic finds a repository locked, the agent or the
  server runs `restic unlock`, which removes stale locks only, and tries once more; the server's daily run removes
  them before it starts, and audits it. A dry run of a PostgreSQL dump reads the whole dump, so restic is no longer cut
  off and leaves no lock behind. `HoldfastCheckFailed` raises a ticket as soon as an agent's repository check fails,
  and says whether the repository was locked.
- Optional off-site copies, on the same pages as primary: a job's off-site retention follows its primary retention
  until one is saved off-site, and Match primary makes it follow again (decision 053). The problems `offsite_copy`
  and `offsite_lag` show under the host and are alerted. Without them, the Overview and Settings say that site B is
  not set up, and nothing else mentions it (decision 057).
- Logins through OIDC with three roles, or `holdfast ui-login` for root without an identity provider. A page opened
  without a session goes straight to the login, without flashing a “not logged in” error on the way, nor a call the
  browser cut off as it left (Firefox's “NetworkError when attempting to fetch resource.”).
- Recovery of the server itself from one recovery file and the bucket.
- Packages for Ubuntu, Debian and the Red Hat family on x86_64, the one-line installer `install.sh`, and the
  Ansible collection `kabsdk.holdfast`, whose agent role sets the PostgreSQL client in `agent.yaml`.
- The documentation, at [kabsdk.github.io/holdfast](https://kabsdk.github.io/holdfast/).
- Tested with restic 0.19.1, with PostgreSQL 16 and 17 as the server's database, and PostgreSQL jobs against
  PostgreSQL 18 with its own client and with the older client 16 (P001), and against PostgreSQL 16 with client 18.
