# 046: The server sends its own alerts

Status: accepted, 2026-11-23 (plan-m10 item 2). You found Holdfast "a VERY complicated product" to run; the review
showed that being told about a failed backup took Prometheus, its rules, a scrape config, a mail script and a dead
man's switch.

## Decision

- **`notify:` in `server.yaml`.** Mail through an SMTP relay (`starttls`, `tls` or `none`, with a user and a password
  file if the relay wants them), a webhook that gets JSON with a `text` field (Slack, Mattermost, Teams and most chat
  tools show it), or both.
- **What it sends.** Every minute the server looks at its Problems, the hosts' and its own. A problem that has lasted
  `notify.after` (10 minutes by default) is sent once; when it ends, that is sent once. The state is in the database,
  so a restart sends nothing twice.
- **The dead man's switch.** After each pass the server calls `notify.heartbeat_url`, so a service outside the site,
  such as Healthchecks.io or Uptime Kuma, notices when the server stops.
- **`holdfast notify test`**, the API's `POST /api/v1/notify/test` and a button on the Settings page send a test alert
  on every channel. A channel that fails is the server's problem `notify`.
- **Prometheus stays.** Its metrics and the rules in `/usr/share/holdfast/prometheus/` are unchanged, for teams that
  have Prometheus already.

## What it costs

The server judges itself. A dead or hung server is caught only by the heartbeat service, which is what the required dead
man's switch did before. Mail and webhooks are simple, with no grouping or silences: Alertmanager remains for that.

## Why

- **The Problems were there already.** The UI's Problems page is the list an operator needs to hear about; sending it
  is a small step, and the same list everywhere means one meaning for "something is wrong".
- **A small team has a mail relay or a chat tool**, not necessarily Prometheus.

## What changes

`internal/server/notify.go`, the table `notify_state`, `server.yaml`'s `notify:`, the installer's `HOLDFAST_MAIL_TO`,
`HOLDFAST_SMTP*`, `HOLDFAST_MAIL_FROM`, `HOLDFAST_WEBHOOK_URL` and `HOLDFAST_HEARTBEAT_URL`, `holdfast notify test`,
the Settings page's Alerts, and metrics `holdfast_notify_*`. docs/alerting.md leads with it.
