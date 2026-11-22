# 042: Who Holdfast is for, and one replaceable server

Status: accepted, 2026-11-22. You asked to "pick a lane" between full high availability and none, and to choose who
Holdfast is for, after comparing it with other products; then "Alright, go".

It narrows decision 030's scope rule and withdraws decision 041's "a second gateway is possible later" as a plan.

## Decision

1. **Who it is for.** A small operations team, one to a handful of people, looking after roughly ten to a few hundred
   Linux servers (Ubuntu, Red Hat Enterprise Linux, Oracle Linux), with S3-compatible storage they already run or rent.
   They want one place to set restic backups up, see them, restore from them and be alerted, instead of cron jobs and
   scripts on each host.

   It is not for Windows or macOS machines, Kubernetes volumes, service providers backing up many customers, fleets of
   thousands, or anyone who needs the backup controller itself to stay up through a failure.

2. **One server, with no high availability.** One holdfast-server, with its PostgreSQL. No clustering, no Kubernetes
   deployment, no standby, no active/passive pair and no second gateway. These are out of scope, not deferred.

3. **While the server is down, backups wait.** Agents keep their schedule and back up as soon as the server answers.
   Nothing is lost: the backups are in the bucket. The storage witness, which lists the bucket directly, alerts when
   snapshots go stale.

4. **Recovery is replacement, not failover.** A new server, given the same bucket, picks up where the old one was. M9
   builds that (decision 040: the nightly export, restoring it, the runbook) and nothing beyond it.

5. **Sizing is stated.** Backup traffic passes through the server, so install.md gives what one server carried in the
   lab, measured, so an admin can see when one server is not enough for a fleet.

## What it costs

- A server outage delays every host's backups until the server is back. For nightly backups, a few hours' outage only
  makes them late; a long one is a gap, which the witness reports.
- A fleet that outgrows one server's network needs a second, independent Holdfast with its own bucket. Holdfast does not
  spread one fleet over several servers.

## Why

- **This is how the class is built.** Every open-source tool whose server sits in the backup path runs one server:
  Kopia's repository server, Proxmox Backup Server, UrBackup, Bareos, restic's own rest-server. Their answer to a lost
  server is a new one on the same storage, or a second one kept in sync and switched to by hand. High availability of
  the backup server appears only in paid enterprise editions: Veeam added an active/passive pair in version 13
  (November 2025), in its Premium edition and on its own appliance only; Bacula's commercial edition documents a
  Pacemaker cluster.
- **Backups tolerate delay; the data must survive.** The data is in S3 with versioning, and optionally in a second
  bucket (decision 041). High availability of the controller would protect something that can be down for hours
  without loss, at the cost of a cluster, a replicated database and a load balancer: about three times what an admin
  runs today. restic itself has no high availability either.
- **The gap Holdfast fills is central control, not uptime.** No open-source tool gives central jobs, a view of every
  host and restores from one place over restic. Backrest is per host, with a sync of history to a central copy;
  Zerobyte backs up mounted shares from one machine, without agents. Kopia's repository server is the closest design
  to Holdfast's gateway: it holds the storage key, and clients pin its certificate's fingerprint. A new Kopia server on
  the same bucket takes over, which is the model for M9.
- **The pull towards high availability came from listing a second gateway as a gap** after M8. It was never a
  requirement.

The comparison, with its sources, was made on 2026-11-22: Backrest's multihost docs, Zerobyte, restic's rest-server,
Kopia's repository server docs and forum, Proxmox Backup Server's docs and a vendor's note on a two-node setup,
UrBackup, Bareos and Bacula Enterprise's docs, and Veeam Backup & Replication 13's release notes.

## What changes

- CLAUDE.md and spec §1 say who Holdfast is for; spec §1.2 lists high availability as out of scope; spec §2.2 says
  backups wait and the server is replaced.
- Decision 041 and plan-m8 no longer list a second gateway as possible later.
- plan-m9 stays the server's whole recovery story, kept small, and gains the sizing measurement.
