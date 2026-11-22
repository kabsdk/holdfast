# 031: Deleting a single snapshot

Status: accepted, 2026-11-21.

## Decision

- **An admin can delete one snapshot.** Holdfast runs restic's own `restic forget <id>`; the space is
  freed at the next prune. It is an action in the UI, the API and the CLI (`holdfast snapshot delete
  <fqdn> <id>`), and it is audited.
- **It waits 7 days, as a pending deletion,** like a retention cut (spec §3.4). During that time it can
  be cancelled. An admin can choose **Delete now** instead, with the same kind of confirmation as Prune
  now.
- **Site B is not touched.** Nothing in site A can delete anything in site B (decision 027). Instead:
  - the vault, which can see site A, marks its copy **Deleted in primary on \<date\>**;
  - it offers **Delete here too**, for a vault admin;
  - that deletion follows the same 7-day rule in B, with its own Delete now.
- **The UI says when the data is really gone.** Deleting a snapshot does not remove its data at once:
  - the space is freed at the next prune;
  - S3 versioning keeps the old files until the bucket's lifecycle rule expires them (decision 030).

  So the snapshot page shows the date by which the data is gone in each site. This matters when the
  reason for deleting is something that should not have been backed up, such as a password.

## Why

- **It is restic's own operation.** It belongs in a restic manager, and it was missing: the only ways to
  remove data were retention, Prune now and decommission.
- **Two deletions by design.** If deleting in A also deleted in B, someone who took over site A could wipe
  B too.
- **One click in B, not automatic.** The alternative was for B to follow A's deletions after N days unless
  a vault admin stopped it. That is less work, but a takeover of A would then wipe B, unless someone
  noticed within N days.

## Where it lands

- **M3:** deletion in site A, together with the pending-prune machinery.
- **M7** (M6 until plan-m5 Q1): the click in site B, with the vault.
