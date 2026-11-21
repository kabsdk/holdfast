# 016: Removal never deletes data, and decommission is separate

Status: accepted, 2026-11-21

## Decision

- **Removal retires, it does not delete.** Removing a job or a host retires the repository with its last
  retention, and no data is deleted.
- **Decommission is the only way to delete a repository's data.** It is a separate action: the operator
  types the FQDN, a grace period applies, and the action is audited.

## Why

Removing a host from the config is routine. Destroying its backups is not, and the two must not be one
click apart. A host that has been removed is often exactly the host someone later needs to restore.

*The reasons were inferred when the record was written. You accepted them on 2026-11-21, leaving the call to me ("Whatever you recommend").*

## Consequences

- Retired repositories stay visible and restorable.
- The length of the grace period is not specified yet.
