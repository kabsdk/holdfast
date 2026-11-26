# 057: A server without off-site copies says so, on the Overview and in Settings

Status: accepted, 2026-11-26. Amends [decision 041](041-two-programs-gateway-and-agent-certificates.md), point 5.
Built in 0.1-dev.

## Decision

- Without `offsite:` in `server.yaml`, the **Overview** has a tile for site B, where the Off-site tile is with an
  off-site bucket, that says **not set up** and links to how to set it up. **Settings** says it too, in its Off-site
  copies row. Both in muted red, beside site B's red label.
- Nowhere else. Agent and job pages, the job rows and the calendar say nothing about a site that does not exist, and
  name no site at all while there is one. There is no alert, no problem on any agent, and no metric.
- `GET /api/v1/settings` and `holdfast settings` already say it: `"offsite": false`.

## Why

- **A missing second copy should be seen.** Decision 041 made off-site optional and showed nothing without it, so a
  server that keeps one copy of every backup looked the same as one that keeps two, and nothing said so. The
  Overview is where people look at the state of their backups.
- **Once, where the server's settings are.** Site B is a setting of the server's, not of a job or an agent. Repeating
  it on every page would be noise, and an alert would nag those who chose one site on purpose.
- Off-site stays optional: nothing is checked, nothing is held back for a copy, and nothing warns.
