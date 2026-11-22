# Off-site copies: holdfast-vault

Status: the direction was decided on 2026-11-21 in
[decision 027](../decisions/027-off-site-copies-by-holdfast-vault.md). The open points are in §7.

Mockup of both UIs: [mockup/server-and-vault.html](../../mockup/server-and-vault.html).

---

## 1. The design

- **A separate program.** `holdfast-vault` runs in site B. It copies each job's new snapshots from site A by
  **pulling** with `restic copy` into site B's repositories, and it prunes those copies.
- **Off-site retention is set in the vault.** A retention setting is a deletion schedule, so whoever sets B's
  retention can delete B's data. Nothing holdfast-server does can delete anything in site B.
- **Each side views the other and changes nothing.** holdfast-server shows the vault's state read-only, and
  the vault shows the server's jobs and primary retention read-only.
- **Every connection is opened by the vault.** Nothing in site A connects to it.
- **It works like holdfast-agent.**
  - It keeps a cached job list.
  - It keeps copying and pruning while holdfast-server is unreachable.
  - It finds new snapshots by listing A's repositories itself.
- **Login is LDAP or OIDC.** Requiring a security key is the provider's decision, which can differ per site.
- **It has its own UI, API and CLI** in the same style as holdfast-server, with red-tinted chrome so it is
  always clear which program you are in.

## 2. Connections, all opened by the vault

| Flow | Content |
| --- | --- |
| The vault pulls from holdfast-server | The list of hosts and jobs, the primary retention, and a read-only S3 key plus the repository passwords for site A's repositories |
| The vault reads site A's storage | Each new snapshot, using `restic copy <id>` with the read-only key |
| The vault reports to holdfast-server | Its jobs, the off-site retention, copied snapshot IDs with the A↔B mapping, lag, pending prunes, retired jobs and the grant log. holdfast-server displays this read-only. |
| Restore grants | holdfast-server posts a request. The vault picks it up on its next poll and returns a read-only key and the repository password for that one restore. |

## 3. Rules in the vault

1. **New jobs are copied automatically.** Their off-site retention starts as a copy of the primary
   retention. It is never linked to it. **Match primary** copies the values again.
2. **If off-site keeps less than primary, the vault warns.** It uses a new code, O101 (proposed), which must
   be accepted. Whether it should block instead is O1.
3. **Retention cuts wait 7 days**, as on primary. R104 turns a cut into a pending prune, and **Keep the longer
   retention** undoes it in one click.
4. **Prune now (off-site)** is for vault admins only. It has the same confirmation as on primary and runs on
   the exact snapshot IDs shown (plan-m1 L2).
5. **Removing a job in holdfast-server stops copying.** Site B keeps the data under its off-site retention
   and shows the job as **Retired**. Deleting it is a **decommission** in the vault: type the FQDN, a grace
   period applies, and it is audited.
6. **Excluding a job in the vault stops copying.** The data is kept.
7. **Restores from site B are granted automatically.** The grant is read-only, covers one restore for a
   limited time, and is logged in both programs.
8. **Site A's maintenance does not prune a snapshot that the vault wants but has not copied yet.**
   holdfast-server knows this from the IDs the vault reports.
9. **B's delete credentials exist only on the vault and in B's escrow bundle.** The vault never accepts code
   from holdfast-server; it is upgraded through site B's own packages.
10. **Every save in the vault is a vault revision**, with its number, who, when, message and accepted codes.
    Revisions can be diffed and are audited, as on holdfast-server.

## 4. The storage floor in site B

- Versioning is on in B's bucket.
- The vault's key can delete objects, which only adds delete markers.
- The vault's key is denied `DeleteObjectVersion` and every call that changes the bucket's configuration.
- A lifecycle rule, owned by B's storage owners, expires noncurrent versions after N days.

## 5. What protects what

| If this happens | Then |
| --- | --- |
| Site A is lost | B is intact and can be restored (see O8). |
| holdfast-server is compromised, or makes a mistake | It cannot delete anything in B: it holds no B credentials and has no say in B's retention. It can stop being a source, which makes the vault alert. |
| A host is compromised | It has no access to B. |
| The vault, or a vault admin's login, is compromised | Cuts still wait 7 days and show as held, though Prune now is possible. The storage floor (N days) is the backstop. |
| The identity provider is compromised | Someone can log in to both programs. The storage floor is the backstop, and the provider can require security keys. |

## 6. The two UIs share one structure

The mockup ([mockup/server-and-vault.html](../../mockup/server-and-vault.html)) follows these rules.

**The same pages, in the same order, with the same names.** Overview, Hosts, Restores, Pending prunes,
Retired, Config history, Audit, Settings. Switching programs keeps you on the same page, host and job.

**Overview is the quick glance.** In both programs it has:
- a one-line summary;
- four cards: Problems, To check, Hosts without issues, Connection;
- a "Needs attention" list, where each item is a plain sentence with one action;
- a status tile for every host.

**Hosts → host → job → snapshots is the path to everything else.** A host is where the agent runs and where
its repository lives.
- **Hosts list:** host status, its jobs, last backup or copy, and repository size, with the filters "Needs
  attention" and "Off-site shorter than primary".
- **Holdfast Server host page:** the agent (online or offline, last seen), the config revision it has applied
  (in sync or drift), software and OS, enrollment progress for a new host, the repository, the host's jobs,
  and host history.
- **Holdfast Vault host page:** the same layout, built around the host's off-site repository.
- **Agent problems are reported once, on the host.** An offline agent or config drift is not repeated on each
  of the host's jobs.

**The same jobs table and the same job page.**
- Columns: Job, Status, Last backup or Last copy, Primary retention, Off-site retention, Off-site vs primary
  (Same, Longer or Shorter).
- The retention that a program cannot change is marked 🔒: off-site in Holdfast Server, primary in Holdfast
  Vault.
- Tables have fixed column widths, so nothing shifts when you switch programs.

**The same status words and colours.**
- Red: Failed, Late. Counted as **Problems**.
- Amber: Shorter, and pending cuts. Counted as **To check**.
- Blue: Running, Copying.
- Grey: Excluded, Not copied.
- Green: OK.
- A disabled button is always grey, so a disabled "Prune now" never looks like an alarm.

**The only visual difference is the chrome.**
- Holdfast Vault: red-tinted background, a red header stripe and a red "VAULT · SITE B" badge.
- Holdfast Server: neutral chrome with a slate "SERVER · SITE A" badge.

**Actions belong to their side.**
- **Holdfast Server:** Back up now, Restore (from either site), primary retention, primary Prune now.
- **Holdfast Vault:** Match primary, Exclude or Include, off-site retention, off-site Prune now.
- **Both:** Keep the longer retention and Decommission, each for its own site.

## 7. Open points

> **Decided on 2026-11-22** by [decision 039](../decisions/039-the-vault-in-detail.md) (plan-m7). O10 went the other
> way from the proposal below: the vault reads site A through restic's `rclone:` backend, not with the same key on both
> S3 servers. O5 is the restore command's 24 hours, with a read-only key per host in B. O7 is 7 days (plan-m5 Q7), and O8
> is B's escrow with plain restic, with no standby (decision 030).

| # | Question | Proposal |
| --- | --- | --- |
| O1 | If off-site keeps less than primary: a warning (O101) or an error? | A warning you must accept, since the vault admin is the authority |
| O2 | The storage floor: how many days N, and is Object Lock used? | N = 30, without Object Lock |
| O3 | Is B a separate storage cluster, or a separate realm on the same Ceph? | A separate cluster, or at least a separate realm |
| O4 | Vault roles: admin only, or viewer as well? What are the group names? | admin and viewer; `holdfast-vault-admins`, `holdfast-vault-viewers` |
| O5 | How long a restore grant is valid | 4 hours, or until the restore ends |
| O6 | When off-site lag alerts | 2 × the job's longest gap |
| O7 | The decommission grace period (shared with primary) | — |
| O8 | If site A is lost entirely, where is a restore from B started? The vault cannot talk to agents. | Through the warm standby in site B. Without one, plain restic with B's escrow bundle. |
| O9 | Do the vault's audit log and alarms also go to syslog? | Yes, where you have one |
| O10 | **How the vault reads site A.** You suggested a dedicated reader service account in site A (`su_holdfast_vault_reader`), with list and get on `repos/*` only. The catch: restic uses one set of `AWS_*` credentials for every S3 repository in a run, so `restic copy` cannot hold an A key and a B key at once (confirmed by a maintainer on the restic forum). | Create the service account on both clusters with the **same access key and secret** (RGW accepts explicitly set keys): read-only in site A, read and write in site B (deletes only add delete markers). Then one set of credentials works for both sides. The fallback is an `rclone:` remote for the source. Also, `--no-lock` on the source would let the A side stay strictly read-only; the M6 test must verify that. |

## 8. Alternatives that were considered

- **Two jobs per host.** Hosts would hold B credentials, B's restore points would differ from A's, and the
  work would double.
- **Storage replication.** It replicates deletions.
- **holdfast-server runs the copies.** The server would hold B's credentials and B's retention, so it could
  delete B's data.
- **Object mirroring.** More custom code, with no integrity check while copying.
- **A vault with two-person approval, a retention lock, clock-tamper checks and separate admins.** More than
  this audience needs. These remain possible later hardening.

## Sources

- Commvault: [air gap](https://documentation.commvault.com/11.42/software/air_gap_for_hyperscale_x.html),
  [WORM storage lock](https://documentation.commvault.com/2024e/commcell-console/locking_retention_and_deletions_with_worm_storage_lock.html),
  [multi-person authorization](https://www.commvault.com/blogs/new-layers-of-data-defense)
- NetBackup: [isolated recovery environment](https://docs.cohesity.com/docs/netbackup/11.0/160799157-169128346-0/v166434544-169128346)
- Veeam: [hardened repository](https://helpcenter.veeam.com/docs/vbr/userguide/hardened_repository.html),
  [time-shift detection](https://www.veeam.com/blog/securing-hardened-repository-against-remote-time-attacks.html)
- Ceph: [archive zone](https://www.ceph.io/en/news/blog/2025/rgw-multisite-replication_part7/)
- Object Lock: [AWS](https://docs.aws.amazon.com/AmazonS3/latest/dev/object-lock-overview.html)
