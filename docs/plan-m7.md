# Plan for M7: off-site copies, and rebuilding Holdfast

Status: accepted, 2026-11-22. You answered the questions in §5 with "Continue", which, as with the earlier plans, I
took as accepting every proposal as written. That decides research/off-site.md's open points O1–O10 (decision 039) and
OQ3 (decision 040). By Q1, M7 is parts 1–4, the vault, and a new M8 is part 5, rebuilding Holdfast; CLAUDE.md's table
changes to match.

M7 is decision 027's `holdfast-vault`, decision 031's "Delete here too", and OQ3's rebuilding of Holdfast. Decision 030
set the frame: copies are made with `restic copy`, there is no standby, and a lost server is rebuilt from a nightly export
to site B, the escrow, and a runbook. The open points are research/off-site.md §7 (O1–O10) and OQ3; §5 puts each to you,
with a proposal.

## 1. What M7 holds

| # | Part | From |
| --- | --- | --- |
| 1 | **holdfast-vault**, a program of its own in site B: it pulls each job's new snapshots from site A with `restic copy`, keeps them under its own off-site retention, and prunes them | 027 |
| 2 | **Its rules**: new jobs copied automatically, off-site retention copied from primary once, the 7-day protection, Prune now (off-site), Exclude, Retired, decommission, "Delete here too" | 027, 031, off-site §3 |
| 3 | **Its own UI, API and CLI**, revisions and audit log, in holdfast-server's style with red chrome | 027, off-site §6 |
| 4 | **holdfast-server's side**: a read-only view of the vault, the hold on pruning what the vault has not copied yet, and restores from site B | 027, 030 §12 |
| 5 | **Rebuilding Holdfast**: a nightly export to site B, a way to bring hosts back from their escrow files, and a tested runbook | 030 §8, OQ3 |

Already decided, and only built here:
- copies by `restic copy`, into B's own repositories with B's own passwords (027, 030);
- off-site retention set in the vault, never by holdfast-server; each side views the other and changes nothing (027);
- every connection opened by the vault; nothing in site A connects to it or deletes anything in site B (027, CLAUDE.md);
- a decommission in the vault waits 7 days, like every other deletion (plan-m5 Q7; this answers O7);
- no standby: if site A is lost, a restore from B uses B's escrow and plain restic (030; this answers O8).

## 2. Design in brief

**1. The vault.** It runs like an agent with a server's face:
- every few minutes it pulls the host and job list, primary retention, and the passwords and a read-only key for
  A's repositories from holdfast-server, and caches them, so it keeps copying while the server is down;
- it lists each A repository itself, and copies the snapshots it has not copied yet: `restic copy --from-repo <A>
  <ids>`. B's repository for a host is made once with `restic init --from-repo <A> --copy-chunker-params`, or
  deduplication is lost;
- once a day it forgets what B's retention drops, behind the 7-day protection, then checks and prunes, as
  holdfast-server does for A (spec §3.4);
- it keeps its state in a PostgreSQL of its own in site B: its revisions, its audit log, and the A↔B snapshot
  mapping.

**2. The rules** are research/off-site.md §3: a new job's off-site retention starts as a copy of primary; **Match
primary** copies it again; a cut waits 7 days (R104), with **Keep longer**; **Prune now (off-site)** for vault admins;
**Exclude** stops copying and keeps the data; a job removed in site A is **Retired** in B, and its data stays until a
vault admin decommissions it; a snapshot deleted in A is marked **Deleted in primary**, and a vault admin may choose
**Delete here too**, which waits 7 days like any deletion.

**3. Its UI, API and CLI** are holdfast-server's, cut down to what the vault does, with red chrome and a "VAULT · SITE
B" badge (the mockup, `mockup/server-and-vault.html`). The rule UI = API = CLI holds inside each program. Off-site
settings exist only in the vault, and holdfast-server shows them read-only: spec §3.3 lists that exception.

**4. holdfast-server's side.**
- The vault reports, on its own polls, what it copies, its lag, its off-site retention and its pending prunes;
  holdfast-server shows them on each host and job, read-only (🔒).
- **The hold:** A's daily run does not forget a snapshot the vault wants and has not copied yet, for a bounded time
  (030 §12), so a stalled B cannot stop A's pruning for ever.
- **A restore from site B** runs on an agent, like M6's restore from another repository: the agent gets B's
  repository, its password and a read-only key, for that one restore (Q4).

**5. Rebuilding Holdfast** (OQ3):
- **Every night holdfast-server writes an export** to its own bucket, under `meta/`: a PostgreSQL dump and the config
  as JSON, encrypted to the escrow's age key (decision 036). The vault copies `meta/` to site B with the rest. So
  nothing in A connects to B.
- **The snapshot catalogue is a cache** (M2): a rebuilt server reads it again from the repositories.
- **With no export at all**, hosts are brought back from their escrow files: `holdfast host import` takes a decrypted
  escrow file and the host's S3 key, and adds the host with its existing repository and password.
- `docs/runbooks/rebuild-holdfast.md`, walked in the lab: a new server VM, the export restored, the agents back.

## 3. Tests

- **e2e:** two MinIO buckets as sites A and B; the vault copies, keeps its retention, prunes, Delete here too, Exclude,
  Retired; holdfast-server's hold; a restore from B; the vault keeps copying while holdfast-server is down; the nightly
  export restored into a fresh database, with the agents back; `host import` from escrow files.
- **Lab:** a second Ceph RGW realm or VM as site B (Q13), the vault VM, the copies of the three agents' repositories, a
  restore from B, and the rebuild runbook on a fresh server VM.

## 4. Order

The vault's copying and holdfast-server's view first; then the vault's retention, Delete here too and the rest of its
rules; then restores from B; then the rebuild. Q1 asks whether that is one milestone or two.

## 5. Questions

**Q1. One milestone, or two?** The vault is a second program, with its own database, UI, API and CLI; the rebuild is
apart from it.
- *Proposal:* split. M7 is the vault (parts 1–4). A new M8 is rebuilding Holdfast (part 5). CLAUDE.md's table changes
  to match.
- *The alternative:* all of it in M7.

**Q2. Site B's own hosts** (030 §15: "where site B's own hosts back up to, and what they do when the sites are cut off
from each other").
- *Proposal:* each site runs its own holdfast-server for its own hosts, into its own bucket, and its own vault for the
  other site's copies. So A's hosts are copied to B by B's vault, and B's hosts to A by A's vault. When the sites are cut
  off, each keeps backing up at home; copies catch up afterwards, and the lag alerts meanwhile (Q7).
- *The alternative:* one direction only (A to B), and B's hosts back up to A's server over the link.

**Q3. How the vault reads site A (O10).** restic uses one set of S3 credentials for both repositories of a `restic
copy` (checked: restic 0.19.1's `copy` takes a separate password and repository for the source, not separate keys). The
vault holds B's write key, and must read A with a key that may only read.
- *Proposal:* reach A through restic's `rclone:` backend, with A's read-only key in rclone's environment, and B through
  `s3:` with B's key. The vault's package ships a pinned rclone, as the agent ships restic. Before anything else, M7
  verifies that a copy works with A's key unable to write (restic's `--no-lock` and the source side).
- *The alternative* (research/off-site.md O10): the same access key and secret on both S3 servers, read-only in A and
  read-write in B. It needs no rclone, but works only where keys can be set by hand (Ceph RGW, MinIO; not AWS), and
  whoever can read A's S3 user database then holds a secret that can write to B.

**Q4. Restores from site B.** Decision 030 dropped "one snapshot, for a limited time": a restore reads the whole
repository. Holdfast never makes S3 keys (decision 029).
- *Proposal:* B's storage admin makes one read-only key per copied host, for its B prefix, and gives it to the vault
  with the host (as with host keys in A). holdfast-server asks the vault for a restore; the vault answers with the B
  repository, its password and that key; holdfast-server sends them to the restoring agent sealed, once, as in M6. Each
  grant is logged in both programs. The agent's restic runs with `--no-lock`.
- *Open detail (O5):* how long a grant is valid. *Proposal:* the restore command's 24 hours (plan-m2 Q6); the key itself
  stays valid, so the runbook says when to make a new one.

**Q5. Off-site shorter than primary (O1).** *Proposal:* a warning, O101, which the vault admin must accept, since the
vault admin decides B's retention.

**Q6. The vault's roles (O4).** *Proposal:* admin and viewer, bound to the groups `holdfast-vault-admins` and
`holdfast-vault-viewers` by default, with the same binding model as holdfast-server (decision 035).

**Q7. When the off-site lag alerts (O6).** *Proposal:* when a job's newest copy is more than twice the job's longest gap
behind its newest backup in A, as the stale alert does on primary.

**Q8. How long the hold lasts.** A's daily run waits to forget a snapshot the vault has not copied yet. *Proposal:* at
most 7 days, then it forgets as usual and alerts; a vault that stalls longer is an incident, not a reason to keep A's
data for ever.

**Q9. The nightly export (OQ3).**
- *Proposal:* as in §2 part 5: holdfast-server writes `meta/<date>.age` to its own bucket every night (a PostgreSQL dump
  and the config, encrypted to the escrow recipient), the vault copies it to B, and the bucket's lifecycle rule ages old
  ones out. Restoring it also needs the server's credentials key, which the admin keeps offline already
  (`holdfast-server keygen`).
- *The alternative:* OQ3's leaning, a `holdfast-meta` bucket with revisions and a hash-chained audit log written as they
  happen. It is more complete, and much more work.

**Q10. Agents after a rebuild (OQ3).** *Proposal:* keep the bearer tokens (plan-m1 Q3): the export holds their hashes,
so agents enrolled before it reconnect by themselves; one enrolled after it gets a new code. *The alternative:* OQ3's
leaning, agent identities from a Holdfast CA whose key is in escrow, so no agent needs a new code.

**Q11. Bringing hosts back without any export (OQ3).** *Proposal:* `holdfast host import <fqdn> --escrow-file <decrypted>
--s3-key-file <key>`, admin only, which adds the host with its existing repository and password, after the storage test,
and audits it. Its jobs are set again by hand, from the config export if there is one.

**Q12. The audit log's integrity (OQ3).** *Proposal:* no hash chain in M7: the audit log is in the nightly export, which
the bucket's versioning keeps. *The alternative:* OQ3's hash-chained log.

**Q13. Site B's storage (O2, O3).** *Proposal:* the vault tests B's storage with the same contract as A's (B001–B011,
docs/storage.md), so versioning and a lifecycle rule are required there too, and N is B's storage admin's choice, as in
A. No Object Lock. The docs recommend a separate cluster for B, or at least a separate realm; Holdfast cannot test
that, and does not.

**Q14. The vault's audit log and alarms in syslog (O9).** *Proposal:* both programs log to the journal, which forwards
to syslog where a site has one; the vault's audit log stays in its own database, as holdfast-server's does, and the
vault exports Prometheus metrics like the server's.

**Q15. The vault's database.** *Proposal:* a PostgreSQL of its own in site B, with the same migrations approach as
holdfast-server. *The alternative:* a local file database, which would make the vault simpler to run and its revisions
harder to export.
