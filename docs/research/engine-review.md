# Engine review: is restic the right choice?

Status: analysis for review, 2026-11-21. Nothing here changes
[decision 001](../decisions/001-restic-only-behind-an-engine-interface.md) until you decide.

**Verdict: keep restic.** No other engine clears all of the gates in §1.

The case for two alternatives may change, so they are worth re-checking:
- **rustic**, if it reaches a production release. It uses the same repository format, so it would need no
  migration.
- **Kopia**, if it gains Linux ACL and xattr support.

There is one concrete change to make: move the pinned version from restic 0.18.1 to **0.19.1**, which has
been current since 2026-07-05, and re-run the PoC tests on it.

---

## 1. What the engine must do

These criteria come from the spec. A **gate** is pass or fail. The **scored** criteria decide between
engines that pass every gate.

| # | Criterion | Kind | Where in the spec |
| --- | --- | --- | --- |
| G1 | Linux metadata fidelity: owner and group; mode, including setuid, setgid and sticky; ns mtimes; POSIX access and default ACLs; xattrs (`user`, `trusted`, `security.selinux`); hard links; symlinks; FIFOs; sparse files | gate | §5.4 ("files keep owner/mode/ACLs"), and the RHEL and Oracle Linux targets, where SELinux labels matter |
| G2 | Writes **directly to S3** (Ceph RGW) from the host, with no server in the data path | gate | §2.1 and §2.2: agents keep backing up during a control-plane outage |
| G3 | Client-side encryption, and restores without Holdfast using a standalone open tool | gate | §2.3 (escrow), decision 001 |
| G4 | OSI open-source licence | gate | §1: free and open source |
| G5 | Production-stable, with a stable repository format | gate | — |
| G6 | Works when the host's key can delete only under `locks/` | gate | §2.3 |
| G7 | Machine-readable output for everything Holdfast shows: backup summary, snapshots, `ls --long`, `find`, `diff`, a dry-run forget with the reason each snapshot is kept, check, restore | gate | §4.2, §5 |
| S1 | Maintenance (prune or GC) that does not block backups | scored | plan-m1 Q7 |
| S2 | Built-in time-window retention | scored | §4.2. Not decisive: Holdfast can compute the set to keep itself and forget exact IDs (plan-m1 L2). |
| S3 | A primitive for off-site copies | scored | OQ1, [off-site.md](off-site.md) |
| S4 | Object Lock and immutability support | scored | OQ1 ("last line") |
| S5 | Packaging on Ubuntu, RHEL and Oracle Linux | scored | §1 |
| S6 | Maturity, governance and longevity | scored | — |

## 2. Candidates

| Engine | G1 fidelity | G2 S3 direct | G4 licence | G5 stable | S1 maintenance doesn't block backups | S3 off-site primitive | S4 Object Lock | Result |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| **restic 0.19.1** | **yes**: all 13 properties proved by the PoC on 0.18.1 | yes | BSD-2 | yes, since 2014 | **no**: prune (and reportedly check) takes an exclusive lock | `restic copy` (re-encrypts) | not native. It works with versioning, because a delete only adds a marker. | **passes every gate** |
| rustic 0.11.2 | restic format, so expected the same; not tested | yes | Apache-2/MIT | **no**: "beta … not yet considered ready for production" (Sept 2026) | **yes**: two-phase, lock-free prune | restic-compatible copy | — | fails G5 |
| Kopia | **no** (community reports): file mode only, no POSIX ACLs or xattrs, so no SELinux labels | yes | Apache-2 | yes | yes | `repository sync-to` (blob mirror) | **native**: compliance mode, and maintenance extends the locks | fails G1 (re-check) |
| Borg 1.4 | yes | **no**: SSH to a borg server | BSD-3 | yes | no (exclusive) | — | append-only mode on the server | fails G2 |
| Borg 2.0 | yes | yes (borgstore: s3, b2, rclone) | BSD-3 | **no**: still 2.0.0b2x, with a breaking repository format | — | `borg transfer` | — | fails G5 |
| Proxmox Backup Server (client + server) | yes: pxar keeps xattrs, ACLs and fcaps | **no**: the client writes to a PBS server, and the S3 datastore is a technology preview (PBS 4.0) | AGPL-3 | yes | yes (server-side GC) | sync jobs, pull or push | no on S3 | fails G2 |
| Plakar | not tested | yes | ISC | young project, with a commercial edition alongside | — | Kloset store sync | — | fails G5 for now |
| Duplicacy | — | yes | **no**: the CLI is free only for personal use | yes | yes (lock-free) | yes | — | fails G4 |
| Bareos / Bacula | yes | through plugins | AGPL-3 | yes | — | copy and migrate jobs | — | not an engine: a whole competing platform (see §3) |
| Duplicity | partial | yes | GPL | yes | — | — | — | its full+incremental chains can't fit snapshot retention |

"—" means not relevant, or not checked because the engine already failed a gate.

## 3. Notes on each candidate

**restic.**
- It is the only candidate that passes every gate, and the PoC proved the hardest one (G1) end to end.
- Its weaknesses are known, and Holdfast already plans around them:
  - **Exclusive lock.** Prune, and reportedly check, take an exclusive lock that blocks backups.
    Mitigation: schedule maintenance inside each host's longest gap, and use `--retry-lock` (plan-m1 Q7).
  - **No Object Lock support.** Mitigation: bucket versioning plus a rule expiring noncurrent versions, and
    optionally Object Lock on the off-site bucket. With Object Lock on, a delete only adds a marker, and
    locked versions survive.
  - **`restic copy` re-encrypts everything it copies**, so off-site copying costs CPU and bandwidth. See
    [off-site.md](off-site.md).
  - **Every repository key has full access.** There are no read-only or append-only keys. The storage
    policy is what limits a host.
- 0.19.0 (2026-06-09) brought faster index loading, lower memory use, and restores that map ownership by
  name. 0.19.1 (2026-07-05) fixed regressions in 0.19.0.

**rustic** is the interesting one.
- **It removes restic's main weakness.** Its prune is lock-free: it marks packs for deletion, removes them in
  a second phase, and does not block backups.
- **It needs no migration.** It reads and writes restic's repository format, so a later switch would not
  move any data. It could even run only the maintenance side.
- **Mixing the two tools on one repository is not proven safe.** That would need its own test.
- **It is not production-ready.** It is still pre-1.0 and describes itself as beta and not ready for
  production.
- **Recommendation:** keep Holdfast's restic engine tied to the repository format, not to the binary, and
  re-check rustic at each milestone.

**Kopia** would be the strongest competitor if it kept Linux metadata.
- **Its strengths fit Holdfast well:**
  - lock-free maintenance;
  - native Object Lock, with locks extended during maintenance;
  - a repository server through which clients never hold storage credentials.
- **Losing ACLs and SELinux labels is disqualifying on RHEL and Oracle Linux.** A restore without labels
  breaks services until the host is relabelled, and lost ACLs go unnoticed.
- **This rests on community reports, not on the docs.** I could not confirm the current status of Kopia
  issue #544. Running the PoC fidelity fixture against Kopia would settle it in about a day.

**Borg.**
- 1.4 has excellent fidelity and a true append-only mode, but it needs SSH to a borg server, not S3.
- 2.0 adds S3 but has been in beta for years, and it breaks the repository format.

**Proxmox Backup Server** is the closest existing product to Holdfast, not just an engine.
- It covers dedup datastores, verify, prune and GC jobs, pull and push sync jobs for off-site copies,
  tokens and ACLs, and tape.
- **It fails G2.** Backups go through a PBS server, which would itself need high availability, or backups
  stop when it is down.
- **Its S3 support is a technology preview**, and its official client is packaged for Debian only.
- If PBS were acceptable, the right question would be whether to build Holdfast at all, not which engine to
  use.

**Bareos and Bacula** are also full platforms, built around job chains and volumes rather than snapshots.
Choosing one would replace Holdfast rather than give it an engine.

## 4. What would change the verdict

| Event | Effect |
| --- | --- |
| rustic ships a production release, and a mixed restic/rustic lock test passes | Consider rustic for maintenance only (lock-free prune) or for everything. No data migration. |
| Kopia passes the PoC fidelity fixture | A real comparison: its Object Lock and its maintenance that does not block backups against restic's maturity. A switch would mean migrating every repository. |
| Borg 2.0 is released as stable | Re-evaluate. Fidelity and append-only are strong, but it has a Python runtime and a new format. |
| PBS makes S3 datastores stable and ships RHEL clients | Still fails G2 because of its server tier. Only relevant if the architecture changes. |

## 5. Proposed follow-ups (not done)

1. **Pin restic 0.19.1**, not 0.18.1, for M1, and re-run all five PoC tests on it (plan-m1 Q10).
2. **Optional bake-off.** Run the PoC fidelity fixture against Kopia and rustic, which turns the community
   reports into facts. It takes about a day and needs root.
3. **Keep the engine interface keyed to the repository format.** That way, a later move from restic to
   rustic would be an implementation change, not a migration.

## Sources

- restic: [0.19.0 release](https://restic.net/blog/2026-06-09/restic-0.19.0-released/),
  [0.19.1 release](https://restic.net/blog/2026-07-05/restic-0.19.1-released/),
  [release notes summary](https://releasebot.io/updates/restic)
- rustic: [GitHub](https://github.com/rustic-rs/rustic),
  [OpenBSD port (status, Sept 2026)](https://www.openbsdhandbook.com/packages/release/sysutils/rustic/),
  [0.11.2 on docs.rs](https://docs.rs/crate/rustic-rs/latest/builds/3100713),
  [lock-free prune (FAQ)](https://docs.rs/crate/rustic-rs/0.5.0/source/FAQ.md)
- Kopia: [ACL support thread](https://kopia.discourse.group/t/kopia-acl-support/787),
  [attributes thread](https://kopia.discourse.group/t/which-attributes-are-preserved-by-kopia/2012),
  [ransomware protection / Object Lock](https://kopia.io/docs/advanced/ransomware-protection)
- Borg: [Borg 2.0 status](https://www.borgbackup.org/releases/borg-2.0.html),
  [2.0.0b22 on PyPI](https://pypi.org/project/borgbackup/2.0.0b22/)
- Proxmox Backup Server: [storage and S3 (technology preview)](https://pbs.proxmox.com/docs/storage.html),
  [client](https://pbs.proxmox.com/docs/backup-client.html),
  [pxar file format](https://pbs.proxmox.com/docs/file-formats.html)
- Plakar: [OVHcloud guide](https://docs.ovhcloud.com/en/guides/storage-and-backup/backup-and-disaster-recovery-solutions/third-party-software/automated-plakar),
  [AlternativeTo](https://alternativeto.net/software/plakar/about)
- restic and Object Lock: [restic forum](https://forum.restic.net/t/any-way-to-get-restic-to-work-with-s3-object-locking-or-a-read-only-key/6858/16),
  [AWS Object Lock](https://docs.aws.amazon.com/AmazonS3/latest/dev/object-lock-overview.html)
- From memory, not re-checked today: Duplicacy's licence terms, Duplicity's chain model, Bareos and Bacula's
  architecture.
