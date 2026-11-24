# 002: Kubernetes is out of scope

Status: accepted, 2026-11-21. **Narrowed by [decision 055](055-postgresql-databases.md) (2026-11-24):** Kubernetes
volumes and API objects stay out, but a PostgreSQL server in Kubernetes is backed up like any database server, by the
agent's container image in a pod, with pg_dump over the network.

## Decision

Holdfast does not back up Kubernetes. That includes PostgreSQL running on CloudNativePG, which keeps its
own Barman Cloud plugin backups.

## Why

- CloudNativePG already has native, supported backups that understand the database, which Holdfast would
  only duplicate.
- Kubernetes workloads do not fit Holdfast's model. There, the unit of protection is volumes and API
  objects. In Holdfast it is a host identified by its FQDN, with one repository.

## Consequences

- No Kubernetes agent, no CSI integration and no CloudNativePG integration.
- Kubernetes nodes may still be enrolled as ordinary Linux hosts for their host files. That case is not
  designed for.
