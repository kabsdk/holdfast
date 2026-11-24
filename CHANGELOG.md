# Changelog

What each release of Holdfast changes. The Ansible collection has its own notes in
[ansible/CHANGELOG.md](ansible/CHANGELOG.md).

## 0.1-dev

The first release, as a development release: **it is subject to change.** Until 0.1.0 is released, each new build
replaces it under the same name and tag, `v0.1-dev`, and installs as an upgrade of the one before (decision 054).

- **holdfast-server**: config revisions in PostgreSQL, enrollment, the HTTP API, the web UI, the gateway through which
  hosts back up without an S3 key, maintenance under the 7-day protection, the storage witness, alerts by mail,
  webhook and heartbeat, Prometheus metrics, and the nightly export.
- **holdfast-agent**: runs restic on each host on its own schedule, through the server's gateway; restores on the
  host; and keeps its events until the server is back. Holdfast runs no command on a host: an export, such as a
  database dump, is the host's own job, on its own timer, and a Holdfast job backs up its files (decision 052).
- **holdfast**: the CLI, with every action the web UI has.
- **The web UI**: an **Overview** of every host, worst first, whose tiles count stale, failed and incomplete jobs and
  filter the hosts by them; a **host page** with a row per job and its last 14 days as squares; a **job page** that
  draws what restic keeps, a row of squares per rule of the retention, and a **calendar** of every kept snapshot;
  and a snapshot's **Restore** as a tree of ticks, restic's `--exclude` or `--include`, in one column.
- Retention by time windows, decided by restic's `forget --dry-run`; checks with stable codes; deleting a snapshot;
  Prune now; rolling a repository back from S3 versions; restoring onto another host.
- Optional off-site copies, on the same pages as primary: a job's off-site retention follows its primary retention
  until one is saved off-site, and Match primary makes it follow again (decision 053). The problems `offsite_copy`
  and `offsite_lag` show under the host and are alerted.
- Logins through OIDC with three roles, or `holdfast ui-login` for root without an identity provider.
- Recovery of the server itself from one recovery file and the bucket.
- Packages for Ubuntu, Debian and the Red Hat family on x86_64, the one-line installer `install.sh`, and the
  Ansible collection `kabsdk.holdfast`.
- The documentation, at [kabsdk.github.io/holdfast](https://kabsdk.github.io/holdfast/).
- Tested with restic 0.19.1 and PostgreSQL 16 and 17.
