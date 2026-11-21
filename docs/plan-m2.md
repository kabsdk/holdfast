# Plan for M2: snapshots, files, changes and restore

Status: accepted, 2026-11-21. You answered the questions in §6 with "Continue", which I took as yes to
every proposal as written. That includes Q2, which touches OQ3: the catalogue keeps tombstones. Say if any
should change.

## Progress

| Done and tested | Tests |
| --- | --- |
| The catalogue (cache plus tombstones), refreshed after each backup, daily and on request. The snapshot list, files, changes (with metadata) and find, all read on the server with `--no-lock` | e2e `TestSnapshotCatalogueAndReads` |
| The banners for a snapshot not made by Holdfast, a snapshot that disappeared, and a restic password added outside Holdfast. Each has a metric, an alert and `holdfast host acknowledge` | the same e2e test, promtool |
| Restore through the agent: into a new root-only directory, keeping all 13 metadata properties; dry run; the host's own bounds in `agent.yaml`; no snapshot of another host; in place with a safety backup; the restore history; expiry after 24 hours | e2e `TestRestoreThroughTheAgent`, integration `TestRestoreRequests` |

| Search: hosts, jobs, and snapshot IDs including tombstones (spec §5.6) | e2e, in `TestSnapshotCatalogueAndReads` |

| **M2's "done when", on the lab VMs** (Q9). `lab/lab test m1`: a backup and a restore through Holdfast keep all 13 metadata properties on Ubuntu 24.04, AlmaLinux 9.8 and Oracle Linux 9.8 (UEK), against Ceph RGW 19.2.3, with SELinux enforcing on the last two and no AVC denials: 39 of 39 | `lab/test-fidelity.sh`, with `lab/fidelity` |

**M2 is complete.** The lab run found two product bugs, both fixed:
- The printed install command did not give the agent restic's path, and the agent had no default. The
  agent and the server now default `restic_bin` to `/usr/libexec/holdfast/restic`, where the package puts
  the pinned restic (decision 032).
- A repository that Holdfast initialised again, after the old one was lost, had its new key flagged as
  added outside Holdfast, and the re-initialisation was not audited. Initialising a repository now makes
  its keys the known ones, and it is audited as `repository.initialised`, with `again` set the second
  time. Covered by `TestRepositoryKeys`.

**Left for M4.** Spec §5.4's "Restore both versions to diff on the host" is two restores of one path,
each with `--include`, into new directories. The UI makes them with two API calls and shows the `diff -u`
command. It needs no endpoint of its own.

## 1. Goal and done when

**Goal.** For each host you can:
- see its snapshots;
- browse a snapshot's files and see what changed;
- find a file across snapshots;
- restore through the agent, from the API and the CLI.

The UI follows in M4 (decision 028).

**Done when** the restore and metadata/ACL fidelity tests pass on the lab's Ubuntu 24.04, AlmaLinux 9 and
Oracle Linux 9 VMs, against the lab's Ceph RGW (CLAUDE.md). The MinIO e2e suite also passes.

## 2. Scope

**In M2**

| Feature | API | CLI |
| --- | --- | --- |
| Snapshot list per host, filterable by job and month (spec §5.2) | `GET /api/v1/hosts/{fqdn}/snapshots` | `holdfast snapshot list <fqdn> [--job] [--month]` |
| Files: one directory of a snapshot, names and metadata only (spec §5.3) | `GET …/snapshots/{id}/files?path=` | `holdfast snapshot files <fqdn> <id> [path]` |
| Changes against the previous or any older snapshot | `GET …/snapshots/{id}/changes?against=` | `holdfast snapshot changes <fqdn> <id> [--against <id>]` |
| Find a file in all snapshots (spec §5.2) | `GET …/find?pattern=` | `holdfast snapshot find <fqdn> <pattern>` |
| Restore, with dry run (spec §5.4) | `POST …/snapshots/{id}/restore` | `holdfast restore <fqdn> <id> …` |
| Restores history, fleet-wide (spec §5.5) | `GET /api/v1/restores` | `holdfast restores` |
| Search: hosts, jobs, snapshot IDs (spec §5.6) | `GET /api/v1/search?q=` | `holdfast search <q>` |

Also in M2:
- **Restore bounds on the host** (decision 030): set in `agent.yaml`, which only root on the host can
  change.
- **Red banners and alerts** for changes made outside Holdfast (decision 032):
  - a snapshot Holdfast did not make;
  - a snapshot that disappeared without Holdfast deleting it;
  - a restic password added outside Holdfast.

**Not in M2**
- Retention, pending prunes and "which rule keeps this snapshot": M3. The snapshot list leaves that
  column empty until then.
- Restore to another host, and rolling back from S3 versions: M5. The "disappeared" banner says until
  when the snapshot can be recovered, and links to the runbook.
- The UI, OIDC and roles: M4. M2 runs on the root-only socket, like M1.

## 3. Design in brief

**Reads run on the server, without a lock.** Snapshot lists, files, changes and find are reads.
- The server runs them with the maintenance key and the host's password, always with `--no-lock`.
- So they work while the host is offline, and they never wait for an agent's check, nor delay a backup.
- B003 already guarantees that the maintenance key can read what the host wrote.
- Only names and metadata leave restic, never file contents (CLAUDE.md).

**The catalogue is a cache** of `restic snapshots --json` per host, in PostgreSQL.
- Refreshed after each backup event, once a day, and on request.
- On each refresh the server compares the snapshot list with what it knew. That comparison drives the
  "not made by Holdfast" and "disappeared" banners.
  - "Made by Holdfast" means the snapshot ID was reported in one of the agent's backup events.
  - "Disappeared" means a known ID is gone, and Holdfast did not delete it. Holdfast's deletions start
    in M3.

**Restore runs on the agent,** as a command in its one-restic-at-a-time loop.
1. The server queues `restore` with the snapshot, the selection and the target.
2. The agent checks the request against its local bounds, then runs `restic restore --sparse --verify`.
   It uses `--include` or `--exclude`, whichever gives the shorter list (spec §5.4).
3. It reports an event of kind `restore`, with counts and the target path.
4. A dry run is `restic restore --dry-run` and reports the same counts.

For an in-place restore, the agent first backs up the paths it will replace (decision 023).

**Changed passwords.**
- Holdfast records the ID of the repository key it creates at init.
- The agent's daily check also runs `restic key list --json` and reports the key IDs.
- An ID Holdfast did not create raises the banner.

## 4. Tests

**e2e (MinIO, in the container):**
- snapshot list;
- files with pagination;
- changes;
- find;
- restore into a new directory, with all 13 fidelity properties identical;
- in-place restore with its safety backup;
- a restore refused by the host's bounds;
- the three banners, each made by hand with plain restic: a manual backup, a snapshot file deleted
  with the maintenance key, an added key.

**On the lab VMs (the "done when").** Run the fidelity fixture on each of `ub24`, `el9` and `ol9`, with
the real server on `hfsrv` and the lab's RGW. Back up through Holdfast, restore through Holdfast, compare.
On AlmaLinux and Oracle Linux, SELinux is enforcing, so the `security.selinux` property is meaningful
there.

## 5. Contradictions found in the spec

1. **§5.2 shows "which rule keeps it" and pending prunes.** Those exist from M3. M2 leaves the column
   empty.
2. **§5.6 finds deleted snapshot IDs.** Holdfast deletes nothing until M3. In M2, "deleted" can only mean
   "disappeared outside Holdfast", which needs tombstones. Q2 covers what the catalogue keeps.
3. **§5.4's default path `/var/restore/<date>-<user>-<snapshot>`.** In M2 the user is the socket's local
   user, so `root`. With OIDC from M4, it is the OIDC subject.

## 6. Questions

Each has a proposal. Say yes, or change it.

**Q1. Do reads run on the server?**
- *Proposal:* yes, with the maintenance key and `--no-lock`, as in §3. They then work with the host
  offline, and they never contend with the agent.
- *The alternative:* the agent answers reads. That would work only while the host is online, and every
  read would wait behind a running backup.

**Q2. What does the catalogue keep?** This touches OQ3, which is open, so I am asking.
- *Proposal:* the cache of `restic snapshots --json`, plus a tombstone for each snapshot that disappeared
  or was deleted: its ID, host, job, time and when it went.
- Everything except the tombstones can be rebuilt from the repositories. Tombstones are lost if
  PostgreSQL is lost.

**Q3. What if a restore and a backup want the host at the same time?**
- *Proposal:* the restore waits for a running backup, in the agent's loop.
- Backups that come due during a restore are skipped and reported, as overlapping slots are today.
- A restore does not interrupt a running backup.
- *Decided (you left it to me):* keep it as built. A backup that comes due during a restore runs once,
  right after the restore. That is the same catch-up rule as after an outage (plan-m1 Q7), and nothing is
  lost. Its slot is not reported as skipped.

**Q4. Where does an in-place restore's safety backup go?**
- *Proposal:* a snapshot of exactly the paths it will replace, tagged with the job and `pre-restore=<restore
  ID>`, kept by the job's retention like any other.
- The restore event links to it, so undoing the restore is one more restore.

**Q5. What are the host's restore bounds, and their defaults?**
- *Proposal:* in `agent.yaml`:
  - `restore.allow_in_place: false`;
  - `restore.allow_other_directories: false`;
  - `restore.root: /var/restore`.
- Plus: only snapshots whose hostname is this host's FQDN.
- A refused restore is reported with the reason. The server shows each host's bounds, so the UI can grey
  out what the host refuses.

**Q6. How long does a queued restore wait for an offline host?**
- *Proposal:* 24 hours. After that it is cancelled, and the cancellation is reported and audited.

**Q7. May a snapshot Holdfast did not make be restored?**
- *Proposal:* yes, if its hostname is this host's. It is shown as "not made by Holdfast" (decision 032),
  but its data is still the host's own.

**Q8. Large directories and slow finds.**
- *Proposal:*
  - files are listed one directory at a time, with pages of 1000 entries;
  - `find` runs on the server with a 5-minute limit, and says when it hit it.

**Q9. How do the lab VM tests run?**
- *Proposal:* `lab/lab test m1 m2`:
  1. it deploys the current build;
  2. it enrolls the three agents against `hfsrv`, using the lab's RGW in the documented layout;
  3. it copies a test binary of the fidelity check (`go test -c`) to each VM, runs it there, and
     collects the results.

  It needs no internet on the VMs.
- *Open point:* `hfsrv` needs `server.yaml`, a TLS certificate signed by a lab CA, and the maintenance
  key. Today the lab provisions none of that; M2 adds it to `lab/ansible/site.yml`.
