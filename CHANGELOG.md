# Changelog

What each release of Holdfast changes. The Ansible collection has its own notes in
[ansible/CHANGELOG.md](ansible/CHANGELOG.md).

## Unreleased

- Web UI: **Overview** replaces the Fleet and Problems pages. Its tiles count stale, failed and incomplete jobs and
  the hosts with other problems, and filter the hosts by them; each problem shows once, under its host, and the
  sidebar's "Needs attention" list is gone.
- Web UI: the **host page** has a row per job, with its last 14 days as squares, its last and next backup, Back up
  now and Restore; the host's rarer actions are in a ⋯ menu, and recent runs fold by job and day.
- Web UI: a **job page** draws what restic keeps, a row of squares per rule of the retention, beside its snapshots,
  and a **calendar** has every kept snapshot, a square a day. Every square shows its date, snapshot and reasons on
  hover.
- API and CLI: `GET /api/v1/hosts/{fqdn}/days` and `holdfast host days <fqdn>` give each job's backups on each of the
  last days, and the worst of each day.

## 0.1.0 (2026-11-24)

The first release. Two development releases before it, 0.5.0 and 0.6.0, were withdrawn when the numbering started
again at 0.1.0; older decision records and comments may still name them.

- **holdfast-server**: config revisions in PostgreSQL, enrollment, the HTTP API, the web UI, the gateway through which
  hosts back up without an S3 key, maintenance under the 7-day protection, the storage witness, alerts by mail,
  webhook and heartbeat, Prometheus metrics, and the nightly export.
- **holdfast-agent**: runs restic on each host on its own schedule, through the server's gateway; restores on the
  host; and keeps its events until the server is back. Holdfast runs no command on a host: an export, such as a
  database dump, is the host's own job, on its own timer, and a Holdfast job backs up its files (decision 052).
- **holdfast**: the CLI, with every action the web UI has.
- Retention by time windows, decided by restic's `forget --dry-run`; checks with stable codes; deleting a snapshot;
  Prune now; rolling a repository back from S3 versions; restoring onto another host; optional off-site copies.
- Logins through OIDC with three roles, or `holdfast ui-login` for root without an identity provider.
- Recovery of the server itself from one recovery file and the bucket.
- Packages for Ubuntu, Debian and the Red Hat family on x86_64, the one-line installer `install.sh`, and the
  Ansible collection `kabsdk.holdfast`.
- The documentation, at [kabsdk.github.io/holdfast](https://kabsdk.github.io/holdfast/).
- Tested with restic 0.19.1 and PostgreSQL 16 and 17.
