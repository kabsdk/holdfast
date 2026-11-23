# Changelog

What each release of Holdfast changes. The Ansible collection has its own notes in
[ansible/CHANGELOG.md](ansible/CHANGELOG.md).

## Unreleased

Breaking:

- **Dump jobs are removed** (decision 052): Holdfast runs no command on a host, and gets no pre-backup hooks either.
  `dumps:` in `agent.yaml`, the `--dump` flag of `holdfast job set`, the UI's "A dump the host defines" and the API's
  `dump` in a job are gone. The checks D001, D002 and I002 are retired. Instead, a systemd service and timer on the
  host write the export, such as `pg_dumpall`'s, to a temporary name renamed only when it succeeded, and a paths job
  backs up its directory; the host's monitoring watches the unit
  ([Databases and other exports](https://kabsdk.github.io/holdfast/#dumps)). To upgrade:
  - after upgrading the server, give each dump job paths (`holdfast job set <fqdn> <job> --path …`) or remove it.
    Until then the server refuses to save the config, and a 0.6.0 agent fails the job's runs;
  - take `dumps:` out of `agent.yaml`. An agent that still finds it ignores it, and logs so;
  - snapshots that dump jobs made can still be restored, into a new directory only.
- **The Ansible collection** needs `holdfast_fqdn` on the server and every host, takes each job in `holdfast_jobs` as
  a mapping of `holdfast job set`'s flags, and no longer takes `holdfast_dumps`
  ([ansible/CHANGELOG.md](ansible/CHANGELOG.md)).

Other changes:

- The documentation is published at [kabsdk.github.io/holdfast](https://kabsdk.github.io/holdfast/), with six more
  pictures of the web UI: a job's snapshots, Changes, Restore, Restores, Config history and the audit log.
- Web UI: a long retention reason or audit entry wraps, instead of making the page scroll sideways.
- Documentation, from a first install by someone using only the docs:
  - the example lines pin the same release in the URL and in `HOLDFAST_VERSION`;
  - the webhook's exact JSON, that it takes Slack's format (Discord through its Slack-compatible URL), and that the
    heartbeat is a GET;
  - how the server trusts a private CA for S3: the operating system's trust store;
  - `bootstrap_admins` applies at every login while listed, not only the first.

## 0.5.0 (2026-11-23)

The first release.

- **holdfast-server**: config revisions in PostgreSQL, enrollment, the HTTP API, the web UI, the gateway through which
  hosts back up without an S3 key, maintenance under the 7-day protection, the storage witness, alerts by mail,
  webhook and heartbeat, Prometheus metrics, and the nightly export.
- **holdfast-agent**: runs restic on each host on its own schedule, through the server's gateway; restores on the
  host; runs database dumps from commands defined on the host; and keeps its events until the server is back.
- **holdfast**: the CLI, with every action the web UI has.
- Retention by time windows, decided by restic's `forget --dry-run`; checks with stable codes; deleting a snapshot;
  Prune now; rolling a repository back from S3 versions; restoring onto another host; optional off-site copies.
- Logins through OIDC with three roles, or `holdfast ui-login` for root without an identity provider.
- Recovery of the server itself from one recovery file and the bucket.
- Packages for Ubuntu, Debian and the Red Hat family on x86_64, the one-line installer `install.sh`, and the
  Ansible collection `kabsdk.holdfast`.
- Tested with restic 0.19.1 and PostgreSQL 16 and 17.
