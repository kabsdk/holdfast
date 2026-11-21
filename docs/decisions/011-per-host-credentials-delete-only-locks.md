# 011: Per-host credentials, deletes only under locks/, and no raw restic console

Status: accepted, 2026-11-21

## Decision

- **Per-host credentials.** Each host has its own repository password and its own S3 key.
- **Bucket protection.** The bucket policy allows deletes only under `locks/`, and bucket versioning is on.
- **Escrow.** Keys are escrowed in an offline escrow bundle.
- **Credentials are never shown** in the UI. Holdfast's rules extend this to the API, the CLI, logs,
  metrics and config revisions.
- **No raw restic console.**

## Why

- **A compromised host stays contained.** It can neither read other hosts' data nor delete its own history.
- **Overwrites are recoverable.** Versioning keeps the previous version of any object that is overwritten.
- **Credentials are recoverable.** The escrow bundle restores access if Holdfast's database is lost.
- **Credentials cannot leak through Holdfast.** A credential that is never displayed or exported cannot be
  copied out of it.
- **A raw restic console would bypass the protections.** It would get around the checks, the 7-day
  protection and the audit log.

*The reasons were inferred when the record was written. You accepted them on 2026-11-21, leaving the call to me ("Whatever you recommend").*

## Consequences

- Prune needs an S3 identity that can delete outside `locks/`. Which identity that is, and who creates the
  per-host keys, is plan-m1 Q4.
- With versioning on, deletes made by prune free no space until the noncurrent versions expire (plan-m1 Q4).
