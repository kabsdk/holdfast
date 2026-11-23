# Alerting

**The server sends its own alerts** (decision 046). Everything it knows is wrong, the Problems page's list (spec
§5.5a), goes by mail, to a webhook, or both: once when a problem has lasted `notify.after` (10 minutes by default),
and once when it ends. That covers backups that stop or fail, a backup the bucket does not hold (the server is the
storage witness, decision 045), snapshots that disappear, a failing nightly export, and the rest.

```yaml
# /etc/holdfast/server.yaml
notify:
  mail:
    smtp: relay.example.internal:587
    tls: starttls                       # starttls (the default), tls (port 465), or none
    # username: holdfast                # with password_file, if the relay wants a login
    # password_file: /etc/holdfast/smtp-password
    from: holdfast@holdfast.example.internal
    to: [backup-team@example.internal]
  webhook_url: https://chat.example.internal/hooks/…   # a JSON POST; Slack and Mattermost show its "text"
  heartbeat_url: https://hc.example.org/ping/…         # called (GET) every minute, for the dead man's switch
  # after: 10m
```

The installer writes it from `HOLDFAST_MAIL_TO`, `HOLDFAST_SMTP`, `HOLDFAST_SMTP_USER`, `HOLDFAST_SMTP_PASSWORD`,
`HOLDFAST_SMTP_TLS`, `HOLDFAST_MAIL_FROM`, `HOLDFAST_WEBHOOK_URL` and `HOLDFAST_HEARTBEAT_URL`. Edit it as root, then
`systemctl restart holdfast-server`.

`holdfast notify test`, or the button on the UI's Settings page, sends a test by every channel that is set. A send that
fails is the server's own `notify` problem, and `holdfast_notify_failed{channel}` is 1; it is tried again a minute
later.

## The dead man's switch

A server that is down, hung or cut off sends nothing, and silence looks like health. So the server calls
`heartbeat_url` after every pass that sent what it had to, about every minute, and a heartbeat service **outside the
site** raises the alarm when the calls stop: [Healthchecks](https://healthchecks.io) (open source; it can run on a
small machine elsewhere) or any service like it. It covers the server, its alerts and the network out.

Test it once: stop holdfast-server, and check that the alarm arrives.

## With Prometheus, optional

For teams that run Prometheus: the server's `/metrics`, on its agent listener
(`https://holdfast.example.internal:8443/metrics`), and the rules that ship with Holdfast,
[deploy/prometheus/holdfast-alerts.yml](../deploy/prometheus/holdfast-alerts.yml), also in `/usr/share/holdfast/`.
Prometheus scrapes the server only, never the agents (decision 009). The listener's certificate comes from Holdfast's
own CA unless you gave one: copy `/etc/holdfast/tls/ca.crt` to the Prometheus host for the scrape's `tls_config`.

The server reports what the agents said and what the bucket holds, as restic reads the repositories with the
maintenance key straight from S3: `holdfast_job_snapshot_in_bucket`, `holdfast_storage_newest_snapshot_timestamp_seconds`,
`holdfast_catalogue_read_timestamp_seconds` and `holdfast_catalogue_read_failed`. When they disagree, the bucket wins.

Each rule has a `severity` label:

| Severity | Meaning | Examples |
| --- | --- | --- |
| `page` | Backups are not happening, or are at risk. Act now. | `HoldfastServerDown`, `HoldfastBackupStale`, `HoldfastBackupNotInBucket`, `HoldfastSnapshotsDisappeared`, `HoldfastRepositoryKeyAdded` |
| `ticket` | Look into it today. | `HoldfastBackupFailed`, `HoldfastAgentOffline`, `HoldfastCatalogueFailing`, `HoldfastEscrowNotWritten` |
| `none` | `Watchdog`: always firing, for the dead man's switch. | |

`make rules-test` checks that the rules parse and pass their unit tests.

**With Alertmanager**, route `severity="page"` to whatever pages, `severity="ticket"` to mail or tickets, and
`Watchdog` to a heartbeat service outside the site, with a short `repeat_interval`.

**Without Alertmanager** (`notify:` does the same without Prometheus, so most teams need neither script):
- [deploy/alerting/holdfast-alert-mail.sh](../deploy/alerting/holdfast-alert-mail.sh) reads Prometheus's
  `/api/v1/alerts` every minute and mails once when an alert starts and once when it stops. It needs `curl`, `jq` and a
  `sendmail` (postfix, or msmtp-mta for a relay). With `HEARTBEAT_URL` set, each run that sees `Watchdog` firing pings
  it. For a pager or a chat, replace its `mailto` function. On the Prometheus host:
  `install -m 0755 deploy/alerting/holdfast-alert-mail.sh /usr/local/bin/holdfast-alert-mail`, the two units below,
  then `systemctl enable --now holdfast-alert-mail.timer`.
- [deploy/alerting/holdfast-deadman.sh](../deploy/alerting/holdfast-deadman.sh), run every 5 minutes from a timer on a
  machine outside the site, mails once when Prometheus cannot be reached or `Watchdog` is not firing, and once when
  that is over. It cannot tell whether the in-site mailer works.

`/etc/systemd/system/holdfast-alert-mail.service`:

```ini
[Unit]
Description=Mail Holdfast's firing alerts

[Service]
Type=oneshot
Environment=PROM=http://127.0.0.1:9090
Environment=TO=backup-team@example.internal
# Environment=HEARTBEAT_URL=https://hc.example.org/ping/<uuid>
ExecStart=/usr/local/bin/holdfast-alert-mail
DynamicUser=yes
StateDirectory=holdfast-alert-mail
Environment=STATE=/var/lib/holdfast-alert-mail/firing.tsv
```

`/etc/systemd/system/holdfast-alert-mail.timer`:

```ini
[Unit]
Description=Mail Holdfast's firing alerts every minute

[Timer]
OnBootSec=1min
OnUnitActiveSec=1min

[Install]
WantedBy=timers.target
```
