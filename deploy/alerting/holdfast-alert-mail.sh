#!/usr/bin/env bash
# holdfast-alert-mail: mails the alerts Prometheus has firing, for sites without Alertmanager
# (docs/alerting.md). Run it every minute from a systemd timer, next to Prometheus. It mails once when an
# alert starts firing and once when it stops, and keeps what it has mailed in $STATE.
#
# With HEARTBEAT_URL set, every run that sees the Watchdog alert firing also fetches that URL: a dead man's
# switch outside both sites (Healthchecks, say) then raises the alarm when the fetches stop.
#
# Needs curl, jq and a sendmail (postfix, msmtp-mta, ...).
set -euo pipefail

PROM=${PROM:-http://127.0.0.1:9090}
TO=${TO:?set TO to the address that gets the alerts}
FROM=${FROM:-holdfast-alerts@$(hostname -f)}
STATE=${STATE:-/var/lib/holdfast-alert-mail/firing.tsv}
HEARTBEAT_URL=${HEARTBEAT_URL:-}
SENDMAIL=${SENDMAIL:-/usr/sbin/sendmail}

mkdir -p "$(dirname "$STATE")"
touch "$STATE"

alerts=$(curl -fsS --max-time 20 "$PROM/api/v1/alerts")

if [ -n "$HEARTBEAT_URL" ] &&
  jq -e '.data.alerts[] | select(.labels.alertname == "Watchdog" and .state == "firing")' >/dev/null <<<"$alerts"; then
  curl -fsS --max-time 10 --retry 2 "$HEARTBEAT_URL" >/dev/null || true
fi

# One line per firing alert: severity, alert, host, the rest of its labels, summary.
now=$(jq -r '.data.alerts[]
  | select(.state == "firing" and .labels.severity != "none")
  | [.labels.severity, .labels.alertname, (.labels.host // "-"),
     (.labels | del(.severity, .alertname, .host) | to_entries | map("\(.key)=\(.value)") | join(",")),
     .annotations.summary] | @tsv' <<<"$alerts" | sort -u)

started=$(comm -13 "$STATE" <(printf '%s\n' "$now" | sed '/^$/d'))
stopped=$(comm -23 "$STATE" <(printf '%s\n' "$now" | sed '/^$/d'))

mailto() {
  { printf 'From: %s\nTo: %s\nSubject: %s\n\n' "$FROM" "$TO" "$1"; printf '%s\n' "$2" | cut -f1,2,3,5; } | "$SENDMAIL" -t
}
if [ -n "$started" ]; then
  pages=$(grep -c '^page' <<<"$started" || true)
  mailto "Holdfast: $(wc -l <<<"$started") alerts firing ($pages page)" "$started"
fi
if [ -n "$stopped" ]; then
  mailto "Holdfast: $(wc -l <<<"$stopped") alerts resolved" "$stopped"
fi

printf '%s\n' "$now" | sed '/^$/d' >"$STATE.new"
mv "$STATE.new" "$STATE"
