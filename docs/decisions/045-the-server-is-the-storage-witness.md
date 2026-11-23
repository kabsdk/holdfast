# 045: The server is the storage witness

Status: accepted, 2026-11-23. Replaces decision 008 and amends 009. The question was why the witness had to be a
separate program on a separate host.

## Decision

`holdfast-freshness` goes, with its list-only key and its policy. The server is the witness:

- **What it reads.** After every backup, and at least once a day, the server lists the host's repository with restic
  and the maintenance key, straight from S3, not through the gateway. That is the snapshot catalogue it keeps anyway.
- **What it compares.** The snapshot the agent last reported for each job, against that listing. When a listing that
  started after the report arrived does not hold the snapshot, the job has the problem `not_in_bucket` (an error), the
  metric `holdfast_job_snapshot_in_bucket` is 0, and `HoldfastBackupNotInBucket` pages. Both times are the server's
  clock, so the agent's clock plays no part. A snapshot that was there and went later is `disappeared`, as before
  (decision 032).
- **What it exports.** Per host and job, the newest snapshot in the bucket; per host, when the repository was last read
  and whether that failed (`HoldfastCatalogueFailing`).

## What it costs

A server bug that invents a success and also misreads the bucket would go unseen. The old witness never protected
against a hacked server either: its keys can write a fresh-looking snapshot file, which is all the witness read.

## Why

- **The question is the same, the answer exact.** The old witness compared object times per host. The server compares
  snapshot IDs per job, which the witness could not, as it could not decrypt the `job=` tag (decision 008's L6).
- **Since decision 041 nothing backs up while the server is down**, so there is no window left for a second machine
  to cover. `HoldfastServerDown`, and the heartbeat of decision 046, cover the server itself.
- **One program, one key fewer, no second machine.** The operator installs and watches one thing.

## What changes

- `holdfast-freshness`, its package, unit and installer role are removed; `holdfast-server`'s package replaces it.
- Metrics `holdfast_job_snapshot_in_bucket`, `holdfast_storage_newest_snapshot_timestamp_seconds{host,job_id}`,
  `holdfast_catalogue_read_timestamp_seconds`, `holdfast_catalogue_read_failed`. Rules `HoldfastBackupNotInBucket` and
  `HoldfastCatalogueFailing` replace `HoldfastStorageStale`, `HoldfastStorageScrapeFailed` and
  `HoldfastWitnessDisagreement`.
- The problem `not_in_bucket`; spec §2.1 and §5.5a.
