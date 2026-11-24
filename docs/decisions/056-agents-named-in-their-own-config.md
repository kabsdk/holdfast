# 056: Agents are named in their own config

Status: accepted, 2026-11-24. Amends [decision 004](004-fqdn-identity-one-repository-per-host.md),
[decision 048](048-host-add-and-job-defaults.md) and [decision 050](050-ansible-collection.md). Built in 0.1-dev.

## Decision

- **What backs up is an agent**: a machine, or a pod that backs up a database server
  ([decision 055](055-postgresql-databases.md)). The list is **Agents**, the CLI is `holdfast agent …`, and the API is
  `/api/v1/agents/…`. `holdfast host …` and `/api/v1/hosts/…` are gone, with no alias.
- **An agent's name is its identity**, as the FQDN was (decision 004): it names its repository (`repos/<name>`), its
  escrow file, and the host in its snapshots. A name is lower-case letters, digits and hyphens, in dot-separated parts
  of at most 63, at most 253 in all, such as `web01.example.com`, `matrix` or `k3s-postgres-main`.
- **The name is set, never read from the machine.** `agent_name` in `agent.yaml` is required and has no default.
  `holdfast-agent enroll --name`, the installer's `HOLDFAST_AGENT_NAME` and the collection's `holdfast_agent_name` set
  it. Nothing reads `hostname -f` for it; the collection's `holdfast_hostname_autodetect` is gone.
- **Holdfast shows where an agent connects from**: the address of its last connection, beside its name.
- **Agents keep their names.** An `agent.yaml` that names the agent as `fqdn:` still loads, and the agent logs that the
  key is now `agent_name`; the collection rewrites it. Renaming an agent is not designed: the collection stops when
  the name it is given is not the one the agent enrolled with.
- **The server keeps its DNS name.** Its URLs and certificates still come from `hostname -f` in the installer, and
  from `holdfast_fqdn` in the collection: that is where agents and people reach it, not an identity in Holdfast.

## Why

- **A pod has no name of its own to read.** Its hostname changes with every restart. An agent that backs up a
  database server from Kubernetes must be named after the server, by whoever deploys it.
- **Explicit, not detected.** A name read from the machine changes when someone renames the machine or fixes its DNS,
  and the agent then becomes another one, with a new, empty repository. A name in the config changes only when
  someone changes it.
- **"Host" no longer fits.** An agent for a database server in a cluster is not a host. "Agent" is what all of them
  are.

## What it costs

- Every install, and every inventory, names each agent: one more setting.
- An agent's name and its machine's may differ. The address it connects from tells them apart.
- Scripts that call `holdfast host …` or `/api/v1/hosts/…` must change. 0.1-dev is a development release, subject to
  change (decision 054), so there is no alias to keep.
