# Alerting

Holdfast does not send alerts itself. The server and the storage witness export metrics, and Prometheus
evaluates the rules that ship with Holdfast,
[deploy/prometheus/holdfast-alerts.yml](../deploy/prometheus/holdfast-alerts.yml). Something then has to
deliver what fires. The UI's Problems page shows what the server knows, but nobody gets woken by a web page.

Each rule has a `severity` label:

| Severity | Meaning | Examples |
| --- | --- | --- |
| `page` | Backups are not happening, or are at risk. Act now. | `HoldfastBackupStale`, `HoldfastStorageStale`, `HoldfastSnapshotsDisappeared`, `HoldfastRepositoryKeyAdded` |
| `ticket` | Look into it today. | `HoldfastBackupFailed`, `HoldfastAgentOffline`, `HoldfastEscrowNotWritten` |
| `none` | `Watchdog`: always firing, for the dead man's switch. | |

`make rules-test` checks that the rules parse and pass their unit tests.

## With Alertmanager

Point Prometheus at it as usual. Route `severity="page"` to whatever pages, `severity="ticket"` to mail or
tickets, and `Watchdog` to a dead man's switch outside both sites (below).

## Without Alertmanager

[deploy/alerting/holdfast-alert-mail.sh](../deploy/alerting/holdfast-alert-mail.sh) reads Prometheus's
`/api/v1/alerts` every minute and mails what started or stopped firing. It needs `curl`, `jq` and a
`sendmail` (postfix, or msmtp-mta for a relay). Install it on the Prometheus host:

```
install -m 0755 deploy/alerting/holdfast-alert-mail.sh /usr/local/bin/holdfast-alert-mail
```

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

Then `systemctl enable --now holdfast-alert-mail.timer`. It mails once when an alert starts and once when
it stops, not every minute. For a pager or a chat, replace the `mailto` function in the script.

## The dead man's switch

Every alert above depends on Prometheus running, evaluating the rules, and something delivering them. If any
of that stops, nothing fires, and silence looks like health. The `Watchdog` rule always fires, so something
**outside both sites** can notice when it stops arriving. Pick one:

- **A heartbeat service** that alarms when pings stop, such as [Healthchecks](https://healthchecks.io)
  (open source; it can run on a small machine outside both sites). Set `HEARTBEAT_URL` in
  `holdfast-alert-mail.service` to its ping URL. Each run that sees `Watchdog` firing pings it. This covers
  Prometheus, the rules, the mailer host and the network between them.
- **A pull check**: [deploy/alerting/holdfast-deadman.sh](../deploy/alerting/holdfast-deadman.sh), run every
  5 minutes from a timer on a machine outside both sites. It mails once when Prometheus cannot be reached or
  `Watchdog` is not firing, and once when that is over. It needs to reach Prometheus's API. It cannot tell
  whether the in-site mailer works.
- **With Alertmanager**, route `Watchdog` to a heartbeat service with a short `repeat_interval`.

Test it once: stop Prometheus, and check that the alarm arrives.

## What Prometheus scrapes

- The server, on its agent listener: `https://holdfast.example.internal:8443/metrics`.
- The storage witness (`holdfast-freshness`), which lists the bucket without going through the server, so
  its staleness alerts fire even when the server is down (decision 030).
- Never the agents (decision 009).
