# 041: Two programs: a gateway in the server, agent certificates, off-site copies by the server

Status: accepted, 2026-11-22. You asked me to make the best choice ("you need to make the best decision here"), after
the study in [research/simpler-architecture.md](../research/simpler-architecture.md) and an independent review of it. It
is built in M8 ([plan-m8](../plan-m8.md)).

It replaces: the per-host S3 keys of decisions 011 and 029; holdfast-vault (decisions 027 and 039); the part of
decision 006 that lets a backup run while the server is down; the bearer token of plan-m1 Q3; and CLAUDE.md's rule
"nothing in site A deletes anything in site B".

## Decision

1. **Holdfast is two programs: holdfast-agent and holdfast-server.** holdfast-vault goes. (The storage witness,
   holdfast-freshness, stays an optional monitor.)

2. **Agents reach storage only through the server.** holdfast-server gets a gateway: restic's REST protocol over HTTPS,
   on the agent listener. The agent's restic uses `rest:https://<server>/…`. No host holds an S3 key.
   - The gateway lets a host read and add to its own repository only, and delete nothing but restic's lock files.
   - It is Holdfast's own code, in the server, on the S3 client the server already uses.

3. **An agent is identified by a key it makes itself.** At enrollment the agent makes a key pair and a self-signed
   certificate on the host; the server pins that certificate, as SSH pins a host key. Every agent connection, to the API
   and to the gateway, is TLS with that client certificate (mutual TLS).
   - Enrollment itself stays as it is: one command with a one-time code, valid 15 minutes, and the server's CA
     fingerprint, which the agent pins.
   - There is no Holdfast CA, nothing to renew, and no bearer token any more. Removing a host, or rotating it after a
     compromise, unpins its certificate.

4. **S3 keys: two per site, whatever the number of hosts.**
   - The gateway key: read and write under `repos/`, delete only under `repos/*/locks/`, never an object version.
   - The maintenance key: as today.
   - `holdfast host add` needs no S3 key. Holdfast still never creates S3 users (decision 029's first half stands).

5. **The server makes the off-site copies.** `server.yaml` names a second bucket, off-site. After each backup the server
   copies the new snapshots there with `restic copy`, into off-site repositories with passwords of their own, kept under
   each job's off-site retention, behind the same 7-day protection.
   - Exclude, the hold (a snapshot is not forgotten in primary before it is copied, for at most 7 days), "Deleted in
     primary" and O101 stay. A restore from off-site runs on the host, through the gateway, read-only.
   - Two sites that each have hosts run a server each, and use the other site's storage as off-site.

6. **The floor stays in storage, in both buckets**: versioning and a lifecycle rule, tested as today, and no Holdfast key
   can delete an object version or change a bucket's settings. Object Lock is the storage admin's option: Holdfast
   reports it, and does not manage locks.

7. **The rule about site B becomes:** off-site copies are forgotten only by their own retention, behind the 7-day
   protection, or by an admin's Prune now or Delete now. Deleting a snapshot in primary never deletes its copy.

## What it costs

- **Backups need the server.** While it is down, agents retry, and back up as soon as it answers; the stale alert covers
  a long outage. A second gateway on another machine (the same program, `role: gateway`) is possible later, since it
  needs only the pinned certificates, which are public, and the gateway key.
- **Backup traffic passes through the server.**
- **One server holds the passwords and the maintenance keys of both buckets.** A hacked server can delete in both, as
  far as S3's old versions allow: the storage window is the protection, in both sites, as it already was in site A.
- **A host's data path changes**, so every agent enrolls again. The repositories stay where they are.

## Why

- **The S3 users were the operator's burden, and the vault a second product to run.** A gateway is how Proxmox Backup
  Server, Kopia's server and Bareos avoid per-host storage users; Veeam's direct mode needs one per agent, as Holdfast
  did.
- **A key the host makes beats a token the server hands out.** The private key never leaves the host, so nothing a
  gateway or a proxy sees can be replayed. Pinning needs no CA. restic's REST backend takes a client certificate and a
  pinned server certificate (checked, restic 0.19.1).
- **The gateway key cannot delete** (checked on MinIO and Ceph RGW 19.2.3), so a bug in the code agents talk to cannot
  destroy a backup.
- **Off-site by `restic copy`, not storage replication**: site B keeps passwords and retention of its own, the copier is
  M7's, tested, and replication was tested on no storage and differs on each.
- **Not Object Lock by default**: against a hacked server, versioning with the lifecycle rule already keeps what was
  deleted for the rule's days, counted from the deletion. Object Lock adds protection against a hacked storage admin,
  and only if locks are extended, which costs an API call per file and can lock mistakes in for good.
- **Not short-lived S3 keys (STS)**: 12 hours at most on Ceph and AWS, and it needs STS set up on the storage.
- **Not `rclone serve restic`**: a second pinned program to ship and supervise, with a password file to keep in sync;
  Holdfast's own gateway knows its hosts and audits.

## Accepted on review, not built now

- Second-admin approval for destructive actions.
- A second gateway.
- A witness on the off-site bucket that also watches snapshot counts.
