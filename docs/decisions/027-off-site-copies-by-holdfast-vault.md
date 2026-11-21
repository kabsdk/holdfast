# 027: Off-site copies are made and governed by holdfast-vault in site B

Status: accepted, 2026-11-21, then reopened by [decision 030](030-simplify-after-the-architecture-review.md):
how copies are made (this vault's `restic copy`, or a mirror of the repository files) is open again.
030 also drops the shared cross-site key (O10) and the "one snapshot, for a limited time" restore grant.
This decides part of OQ1. The remaining points are listed in
[research/off-site.md](../research/off-site.md) §7.

## Decision

**A separate program**
- `holdfast-vault` runs in site B. It copies each job's new snapshots out of site A by **pulling** with
  `restic copy`, and it prunes the copies in site B.
- It works like `holdfast-agent`. It keeps a cached job list, keeps copying and pruning while
  `holdfast-server` is unreachable, and finds new snapshots by listing A's repositories itself.

**Who controls what**
- **Off-site retention is set in holdfast-vault, not in holdfast-server.** This replaces OQ1's earlier
  leaning, which put off-site retention in the protection item.
- **Each side can view the other's jobs and retention, but not change them.**
- **Every connection is opened by the vault.**

**Jobs**
- **New jobs are copied automatically.** Their off-site retention starts as a copy of the primary retention
  at the moment the vault first sees the job. It is a copy, never a link, so later changes to primary do not
  follow. "Match primary" in the vault copies the values again.
- **Removing a job in holdfast-server only stops copying.** Site B keeps the data under its off-site
  retention. Deleting it is an action in the vault.

**Restores from site B**
- holdfast-server asks for read-only access to one snapshot, for one restore. The vault grants it
  automatically, with no approval, and the grant is logged on both sides.

**Login and UI**
- The vault login supports LDAP and OIDC. Whether a site requires a security key is the OIDC provider's
  decision.
- The vault has its own UI, API and CLI. They use the same style as holdfast-server, but the vault is
  visibly different: red-tinted chrome.

## Why

- **A retention setting is a deletion schedule.** If holdfast-server set site B's retention, a compromised
  or mistaken server could delete B's data, even if only 7 days later. With retention in the vault, nothing
  holdfast-server does can delete anything in site B.
- **Everything stays visible from both sides.** Viewing in both directions keeps the two aligned easily
  without giving either side power over the other.
- **The security model stays reasonable.** The separation comes from a different program, a different site,
  outbound-only connections, and a different login binding. It needs no two-person rules and no special
  hardware.
- **Restores are not destructive.** Requiring approval for them would make disaster recovery harder for no
  gain in safety.

## Consequences

- **Off-site settings are an exception to UI = API = CLI across programs.** They exist only in the vault's
  UI, API and CLI, and holdfast-server shows them read-only. spec §3.3 must list this exception when M6
  lands.
- **The vault's independence from the identity provider depends on how the provider is set up.** If someone
  can log in as both a Holdfast admin and a vault admin, the storage floor in site B is the backstop
  (research/off-site.md §4).
- **holdfast-server needs a rule for site A:** maintenance must not prune a snapshot that the vault wants but
  has not copied yet.
- **The vault needs its own revisions and audit log,** on the same model as holdfast-server.
