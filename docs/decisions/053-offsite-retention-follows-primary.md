# 053: The off-site retention follows primary's, unless a job has its own

Status: accepted, 2026-11-24. Amends [decision 039](039-the-vault-in-detail.md) and spec §3.7.

## Decision

- **A job's off-site retention is the same as its primary retention, and follows every change there.** It is no
  longer a copy made once, when the job is created.
- **A retention saved off-site is the job's own** (`own_retention` on the off-site job), and stays as it is when
  primary's changes. It may keep more than primary, as an archive in the other site, or less, which is warning O101,
  accepted as before.
- **Match primary** makes the off-site retention primary's again, and from then on it follows primary again.
- The checks are those of any change. A cut that follows primary is protected by the 7-day rule off-site as it is in
  primary: nothing a retention in force during the last 7 days keeps is forgotten (spec §3.4).

## Why

- **A copy made once was a trap.** Lengthening primary's retention left the off-site copies at the old, shorter
  one. The host page showed "keeps less than primary", but nothing alerted, and nobody had chosen it.
- **The usual wish is a mirror**: the same snapshots in the other site, a few minutes behind. That is now what a job
  gets without doing anything. Keeping off-site longer, or shorter, is a choice someone makes, and the job then shows
  it.
- **Explicit, not guessed**: whether the retention is the job's own is a setting the save makes, not something worked
  out from whether two retentions happen to be equal.

## What it costs

- A cut in primary cuts off-site too, after the same 7 days, for every job without its own retention. Someone who
  wants the off-site copies to outlive a primary cut gives the job its own off-site retention.
