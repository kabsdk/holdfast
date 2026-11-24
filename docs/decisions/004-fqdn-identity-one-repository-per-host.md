# 004: The FQDN is the host identity, with one repository per host

Status: accepted, 2026-11-21. **Amended by [decision 056](056-agents-named-in-their-own-config.md) (2026-11-24):**
the identity is the agent's name, set in its `agent.yaml` and never read from the machine. A machine's FQDN is one such
name; `matrix` or `k3s-postgres-main` is another. One repository per agent, under `repos/<name>`, stays. And with
[decision 055](055-postgresql-databases.md), an agent may stand for a database server rather than a machine.

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

## Consequences

- Retention and forget are applied per job, using the `job=<id>` tag.
- Renaming or re-addressing a host is not designed yet.
