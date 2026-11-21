# 004: The FQDN is the host identity, with one repository per host

Status: accepted, 2026-11-21

## Decision

- A host is identified by its FQDN.
- Each host has one restic repository, on S3 (Ceph RGW), under the prefix `repos/<fqdn>`.
- All jobs on a host share that repository. Each job's snapshots are tagged `job=<id>`.

## Why

- **The FQDN is already unique and meaningful.** Operators know it, DNS enforces it, and it names the
  storage prefix directly. That makes the bucket readable without Holdfast, which matters for the witness
  and for recovering from escrow.
- **Jobs on one host deduplicate against each other.** Sharing one repository avoids storing the same data
  twice.
- **Credentials are scoped to one repository.** One repository per host means one repository password and
  one S3 key per host. A compromised host exposes only its own data.

*The reasons are inferred. Confirm or correct them.*

## Consequences

- Retention and forget are applied per job, using the `job=<id>` tag.
- Renaming or re-addressing a host is not designed yet (plan-m1 Q9).
