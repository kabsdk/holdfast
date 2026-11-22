# Open questions

None of these is decided, so do not decide or implement any of them. Where a leaning is recorded, it is a
leaning only.

When a milestone touches one of these questions, the milestone plan names it and asks; it does not work
around it. M7 (off-site and rebuilding Holdfast; M6 until plan-m5 Q1) waits until the off-site and rebuild
questions are decided.

---

## OQ1. Off-site copies

> **Largely decided on 2026-11-21:** see
> [decision 027](decisions/027-off-site-copies-by-holdfast-vault.md).
> - holdfast-vault in site B pulls the copies, and off-site retention is set in the vault.
> - holdfast-server and holdfast-vault can view each other but change nothing.
> - Restores from site B are granted automatically.
>
> The remaining points are in [research/off-site.md](research/off-site.md) §7. The text below is kept as the
> history of how the question was framed.
>
> **Reopened and closed again on 2026-11-21 ([decision 030](decisions/030-simplify-after-the-architecture-review.md)):**
> copies are made with `restic copy`, as in decision 027. A mirror of the repository files was considered
> and rejected: it breaks when B keeps other snapshots than A.

**Original leaning**
- A second repository per host in site B, filled with `restic copy`.
- The copy is run by a centrally managed **copier** agent role in site B. The copier pulls, so site A holds
  no site-B credential.
- Off-site retention is set in the protection item. It defaults to "same as primary" and is never shorter.
- The copier enforces the 7-day protection locally.
- Storage versioning or object lock is the last line of defence.

**Not decided:** push or pull.

**Known constraint:** the destination repository must be created with
`restic init --from-repo … --copy-chunker-params`, or deduplication is lost.

**Review feedback (2026-11-21)**
1. Concern: a copier means an extra host and special logic, so off-site would no longer be centrally
   managed.
2. Then reconsidered: a copier may be acceptable. But B cannot be fully separated from A without also
   cutting off control and metrics, so the two must stay connected for configs and diffs.
3. A copier must keep working without the server, as `holdfast-agent` does.

**Analysis:** [research/off-site.md](research/off-site.md) (third revision, 2026-11-21) shows how Commvault,
NetBackup, Veeam and Cohesity do it, compares a vault in B with storage-enforced protection, and lists
decisions D1–D10 for you to make. Nothing is decided.

## OQ2. A "Prunes" history list

> **Decided on 2026-11-21 (plan-m3 Q3; you left it to me):** yes. Each host has a list of its prunes, both
> scheduled ones and Prune now. Each entry shows when, which kind, who, how many snapshots were forgotten and
> the bytes freed. Built in M3: `holdfast prunes [<fqdn>]`, `GET /api/v1/hosts/{fqdn}/prunes` (spec §3.4).

A list of prunes, both manual (Prune now) and scheduled, under Config history.

**Touches:** M3, the Prune now records.

## OQ3. Rebuilding Holdfast from S3 if PostgreSQL is lost

**Decided around it (decision 030):**
- There is no warm standby.
- A nightly PostgreSQL dump and a config export go to site B, and the sealing key is in escrow.
- A tested rebuild runbook covers the rest.

The points below are still open.

**Leaning**
- The snapshot catalogue is a cache that is rebuilt from the repositories.
- Config revisions and a hash-chained audit log are also written to a versioned `holdfast-meta` bucket.
- Agent identities come from a Holdfast CA, whose key is in the escrow bundle.

**Touches:**
- M1: agent identity and authentication, and the audit log format.
- M2: what the catalogue holds.
- M3: where deleted snapshot IDs are recorded.

## OQ4. Custom roles

> **Decided on 2026-11-22 (plan-m4 Q8; you answered "Go"):** no custom roles. See
> [decision 035](decisions/035-no-custom-roles.md).

Whether admins can define custom roles from the permission names.

**Touches:** M4.
