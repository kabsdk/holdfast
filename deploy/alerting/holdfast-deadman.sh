#!/usr/bin/env bash
# holdfast-deadman: the simplest dead man's switch (docs/alerting.md). Run it every 5 minutes on a machine
# OUTSIDE both sites. It mails once when it cannot reach Prometheus, or Prometheus has no firing Watchdog
# alert, and once when that is over. It cannot tell whether the in-site mailer works; a heartbeat service
# fed by holdfast-alert-mail's HEARTBEAT_URL can.
#
# Needs curl, jq and a sendmail.
set -uo pipefail

PROM=${PROM:?set PROM to Prometheus URL, such as https://prometheus.example.internal}
TO=${TO:?set TO to the address that gets the alarm}
FROM=${FROM:-holdfast-deadman@$(hostname -f)}
STATE=${STATE:-/var/lib/holdfast-deadman/down}
SENDMAIL=${SENDMAIL:-/usr/sbin/sendmail}

mkdir -p "$(dirname "$STATE")"

mailto() { printf 'From: %s\nTo: %s\nSubject: %s\n\n%s\n' "$FROM" "$TO" "$1" "$2" | "$SENDMAIL" -t; }

if curl -fsS --max-time 20 "$PROM/api/v1/alerts" |
  jq -e '.data.alerts[] | select(.labels.alertname == "Watchdog" and .state == "firing")' >/dev/null 2>&1; then
  if [ -e "$STATE" ]; then
    rm -f "$STATE"
    mailto "Holdfast alerting is back" "Prometheus at $PROM answers again, and its Watchdog alert fires."
  fi
elif [ ! -e "$STATE" ]; then
  date -u +%FT%TZ >"$STATE"
  mailto "Holdfast alerting is DOWN" "Prometheus at $PROM cannot be reached, or its Watchdog alert is not firing.
Until this is fixed, no Holdfast alert reaches anyone. Agents keep backing up on their own."
fi
