# A simpler Holdfast: two programs, a gateway, Object Lock

Status: study, 2026-11-22. **Decided by [decision 041](../decisions/041-two-programs-gateway-and-agent-certificates.md)**,
which takes the gateway, makes the off-site copies in the server (proposal B's shape) and does not require Object Lock.
A review found errors in this study, which the decision corrects: Object Lock adds less against a hacked server than
§3.1 and the tables say (versioning with the lifecycle rule already keeps deleted files); reusing the agent's token at
the gateway would be unsafe; the gateway is not stateless; proposal A's "site A cannot delete in site B" does not hold
for a mirror; and the S3 user counts leave out restore keys and the witness. The text below is as it was written.

You asked for three things:
1. **Two programs**, holdfast-server and holdfast-agent, with the server running both sites, so holdfast-vault goes.
2. **No S3 user per host**: today 100 hosts need about 100 S3 users (one bucket per site, one key per host).
3. **Still better than the field**, in features and in security.

This study checked each one by experiment where it could, and by research where it could not.

## 1. What the experiments showed

All with restic 0.19.1 and rclone 1.76.0, the versions Holdfast pins. Scripts in the session's scratchpad; results below.

| # | Question | Where | Result |
| --- | --- | --- | --- |
| 1 | Does restic work on an **S3 Object Lock** bucket, and does the lock hold? | MinIO, and the lab's Ceph RGW 19.2.3 | **Yes, on both.** init, backup, forget, prune and check all work. The files prune deletes stay stored as locked versions. A key that is even allowed `s3:DeleteObjectVersion`, and on Ceph the bucket's owner, cannot delete them ("forbidden by object lock"). A lock can be extended; shortening it is refused. |
| 2 | Can one S3 user hand each host **short-lived credentials** limited to its folder (STS)? | MinIO | **Yes.** restic works with them (`AWS_SESSION_TOKEN`). A host cannot reach another host's repository and cannot forget (both 403). MinIO issued credentials valid for 8 days. Not tested on Ceph. |
| 3 | Can a **gateway** with one S3 key replace the per-host keys? (`rclone serve restic --private-repos --append-only`) | MinIO | **Yes.** Each host has a login of its own, no S3 user. A host cannot reach another host's repository, and cannot forget (403); it can remove stale locks. The files land exactly where Holdfast puts them today, so the server forgets, checks and prunes them directly, and switching needs no data migration. 400 MB took 1.8 s through the gateway against 2.7 s direct (one machine: no CPU penalty; network speed still to measure). Quirk: the gateway's key must also read the key `repos` itself, not only `repos/*`. |

## 2. What research showed

- **Other products** (sources in §7). Two families:
  - *A gateway in the data path*: Bareos/Bacula (the Storage Daemon holds the S3 credentials), Commvault (MediaAgents), Kopia (its Repository Server, with per-user access lists), Proxmox Backup Server (clients push with API tokens; version 4 can keep the data on S3), Veeam in gateway mode. The storage sees one user.
  - *Direct to storage*: Veeam's direct mode, and Holdfast. Veeam's own staff say that without one IAM user and key per agent, every agent can reach every other machine's backups; Veeam offers to create those for you through IAM/STS.
- **STS lifetimes**: Ceph RGW's AssumeRole is capped at 12 hours (hard-coded); AWS roles at 12 hours, AWS federation tokens at 36 hours; MinIO allows days. Ceph has had bugs in how it applies session policies.
- **Object Lock** counts from when a file is written. A file restic has used for two years has an expired lock unless something extends it. Kopia does that in its maintenance (`--extend-object-locks`); restic does not.
- **Bucket replication** (AWS; Ceph multisite and MinIO are similar): deleting a version is never replicated; delete markers are replicated only if you turn that on; with Object Lock, both buckets need it, and delete markers are not locked, only the versions behind them.
- **Veeam** adds four-eyes approval (a second admin approves deletions and shortening immutability) and immutable repositories. Its own docs say four-eyes cannot protect against a compromised Veeam server.

## 3. The building blocks

### 3.1 Object Lock, kept current by Holdfast

- The bucket in each site has Object Lock on, with a default lock of N days. That is a storage setting.
- Holdfast's part, three small pieces:
  - a storage check that the lock is on, as it checks versioning today;
  - once a day, after its maintenance, the server **extends the lock of every file restic still uses** to N days ahead (`s3:PutObjectRetention`), so every file a restore could need stays locked, however old;
  - the host page shows the guaranteed window.
- **What it changes:** a hacked Holdfast server, or a hacked storage login, can no longer destroy anything a restore needs for N days. It can still read every backup (it holds the passwords), and still stop new backups (the storage witness alerts when no fresh snapshot arrives).
- **What it costs:** every file prune replaces stays stored for N days, so the bucket holds N days of churn on top of the live data. A mistake (2 TB backed up by accident) stays for N days, and in compliance mode nobody can remove it early.

### 3.2 A gateway instead of per-host S3 users

- holdfast-server gets a **gateway role**. By default it runs in the server, and the same binary can run it on more machines.
- The gateway holds one S3 key per site. Each agent logs in with the token it already has; there is no S3 user per host.
- It allows each host only its own repository, and no deletes except restic's lock files: today's host policy, enforced by the gateway instead of by S3.
- The server keeps its maintenance key and forgets, checks and prunes directly, as today.
- **S3 users per site, whatever the number of hosts:** 2 (maintenance and gateway).
- **What it costs:**
  - Backups now need a gateway to be up. That gives up part of decision 006, "the agent keeps backing up while the server is down". The gateway keeps no state of its own, so a second one on another machine covers a single failure: with two, a server that is down costs reports and clean-up, as today, and one gateway that is down costs nothing. With the server's gateway alone, a server that is down stops backups.
  - All backup traffic passes through the gateways.
- **Why not short-lived credentials instead:** on Ceph and AWS they last 12 hours at most, so a server outage of half a day stops backups. Ceph has had bugs in enforcing the limits, and Holdfast would become an issuer of credentials, against decision 029. They stay an option for MinIO-only sites.

### 3.3 Off-site without holdfast-vault

Two ways, which differ in what site B is.

| | (a) The storage replicates | (b) holdfast-server copies |
| --- | --- | --- |
| How | The bucket replicates to site B (Ceph multisite, MinIO, AWS); site B's bucket has Object Lock | The server runs `restic copy` into site B's own repositories: the vault's copier, moved into the server |
| Site B's passwords | The same as site A's (an exact mirror) | Their own |
| Site B's retention | Site A's, plus the lock window. With delete markers not replicated, B keeps everything for ever instead | Its own (for example monthly copies for 7 years) |
| Who connects to site B | The storage, storage to storage | Site A's server, with a key that can write and delete in B |
| A hacked site A server | Cannot reach B. Deletions arrive as delete markers, and B's locked versions stay | Can delete in B, through restic; B's locks keep every needed file for N days |
| Holdfast's part | Check that B is fresh (the witness reads B), restore from B | Copy, retention and prune for B, restore from B |
| Fits CLAUDE.md's "nothing in site A deletes anything in site B" | Yes | **No**: the rule would change, relying on B's locks instead |

## 4. Three architectures

| | Today | Proposal A | Proposal B |
| --- | --- | --- | --- |
| Programs | agent, server, vault | agent, server (gateway role) | agent, server (gateway role) |
| Off-site | the vault pulls with `restic copy` | storage replication | the server pushes with `restic copy` |
| S3 users, 100 hosts | ~102 in site A, 2 in site B | 2 in site A, 1 read-only in site B | 2 in site A, 1 in site B |
| S3 work per new host | a user, a policy, a key | none | none |
| Server down | backups go on | backups go on only through a second gateway | backups go on only through a second gateway |
| All gateways down | n/a | backups stop | backups stop |
| Immutability | none required | Object Lock, extended daily, both sites | the same |
| Hacked host | own folder only, no deletes | the same, enforced by the gateway | the same |
| Hacked server: delete needed data | yes in A (versioning is the net); no in B | no, for N days, in either site | no, for N days, in either site |
| Hacked server: read all backups | yes | yes | yes |
| Site B passwords and retention | its own | site A's | its own |
| Holdfast code | as is | less: no vault, no per-host keys | about the same: the copier moves, per-host keys go |

## 5. Against the field

| | Holdfast, proposal A | Veeam (direct) | Kopia (server) | Proxmox Backup Server | Bareos |
| --- | --- | --- | --- | --- | --- |
| Per-host isolation without per-host storage users | yes (gateway) | no (needs an IAM user per agent, or a gateway) | yes | yes | yes |
| Backup data path | gateways, several | direct | the server | the server | Storage Daemons |
| Immutability kept for data still in use | yes (lock extension) | yes | yes (`--extend-object-locks`) | not checked | not checked |
| Deletions wait 7 days, then S3's window | yes | not checked | no | not checked | not checked |
| The host bounds its own restores | yes | not checked | no | not checked | not checked |
| Freshness checked straight from the bucket | yes (witness) | not checked | no | not checked | not checked |
| Four-eyes approval | not yet (Q6) | yes, since 12.1 | no | not checked | not checked |
| Every action in UI, API and CLI, audited | yes | not checked | partly | not checked | not checked |

"Not checked" means this study did not research it, not that the product lacks it.

**Fair reading:** proposal A would match the strongest on isolation and immutability, and add things others do not offer: a host that bounds its own restores, waits before any deletion, and a freshness witness independent of the server. To claim more, it would also need four-eyes approval (Q6, small) and an independent security review.

## 6. Questions

**Q1. How hosts reach S3.**
- *Proposal:* the gateway (§3.2), run in the server and on a second machine by default.
- *The alternatives:* short-lived credentials (MinIO only, in practice), or keeping per-host keys with the provisioner hook.

**Q2. Which gateway.**
- *Proposal:* Holdfast's own, a restic REST endpoint in holdfast-server over the S3 client Holdfast already uses. It checks the agent's own token and serves only that host's repository, append-only. It is a transport, not restic logic, and it reports in Holdfast's metrics and audit log.
- *The alternative:* `rclone serve restic`, pinned, with a password file Holdfast keeps in sync. It works today (experiment 3), with its quirk.

**Q3. Off-site.**
- *Proposal:* (a), the storage replicates, with Object Lock in site B. Least code, a standard storage feature, and site A cannot delete in site B.
- *The alternatives:* (b), the server copies, for site B passwords and retention of its own; or keeping holdfast-vault.

**Q4. Object Lock.**
- *Proposal:* required in both sites; governance mode by default, compliance as the admin's choice; N = 30 days; the server extends locks daily.
- *The alternative:* recommended, not required.

**Q5. The rule "nothing in site A deletes anything in site B".** Only if Q3 is (b). *Proposal:* keep the rule, which (a) keeps.

**Q6. Four-eyes approval.** *Proposal:* an option in `server.yaml`: Prune now, Delete now, rollback, decommission and accepting a retention cut need a second admin's approval.

**Q7. M7's vault.** *Proposal:* keep it until the replacement passes its e2e and lab tests, then remove it in the same milestone.

**Q8. Order.** *Proposal:* this becomes the next milestone, before M8 (rebuilding Holdfast), since the rebuild depends on where site B's data lives.

## 7. Sources

- Veeam: [direct connection permissions](https://helpcenter.veeam.com/docs/backup/agents/object_storage_direct_access.html), [how backup to object storage works](https://helpcenter.veeam.com/docs/vbr/cloud/cc_object_storage_types.html), [forum: an IAM user per agent](https://forums.veeam.com/viewtopic.php?p=478175), [four-eyes authorization](https://helpcenter.veeam.com/docs/backup/vsphere/four_eyes_authorization.html)
- [Commvault: cloud library credentials](https://documentation.commvault.com/v11/software/adding_credentials_to_cloud_library.html), [Bareos: storage backends](https://docs.bareos.org/TasksAndConcepts/StorageBackends.html), [Kopia: repository server](https://kopia.io/docs/repository-server/), [Kopia forum: object lock extension](https://kopia.discourse.group/t/kopia-malware-and-object-locks/2550), [Proxmox Backup Server: user management](https://pbs.proxmox.com/docs-2/user-management.html), [PBS 4.0 beta: S3 datastores](https://forum.proxmox.com/threads/proxmox-backup-server-4-0-beta-released.168834/)
- [restic rest-server](https://www.github.com/restic/rest-server), [rclone serve restic](https://rclone.org/commands/rclone_serve_restic), [restic issue 2202: Object Lock and Content-MD5](https://gh.hhj520.top/restic/restic/issues/2202)
- AWS: [Object Lock](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lock.html), [what replication replicates](https://docs.aws.amazon.com/AmazonS3/latest/userguide/replication-what-is-isnot-replicated.html), [GetFederationToken](https://docs.aws.amazon.com/STS/latest/APIReference/API_GetFederationToken.html), [per-user home directories](https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_examples_s3_home-directory-console.html)
- Ceph: [STS in RGW](https://docs.ceph.com/en/reef/radosgw/STS/), [tracker: STS max session duration](https://tracker.ceph.com/issues/63109), [Red Hat bug: session policy evaluation](https://bugzilla.redhat.com/show_bug.cgi?id=2284154), [multisite replication](https://www.ceph.io/en/news/blog/2025/rgw-multisite-replication_part5/)
- [MinIO: STS AssumeRole](https://docs.min.io/aistor/developers/security-token-service/assumerole/)
