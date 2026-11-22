# Plan for M8: two programs

Status: decided, 2026-11-22. You asked me to make the choice; it is [decision 041](decisions/041-two-programs-gateway-and-agent-certificates.md).
This plan is how it is built. §6 lists what I chose inside that, so you can overrule any of it; nothing waits on an
answer.

## 1. Where it ends

| | Before M8 | After M8 |
| --- | --- | --- |
| Programs | holdfast-agent, holdfast-server, holdfast-vault | holdfast-agent, holdfast-server |
| S3 users, 100 hosts | ~102 in site A, up to ~102 in site B | 2 per bucket |
| Adding a host | make an S3 user and key, then `holdfast host add --s3-key-file` | `holdfast host add` |
| On the host | an S3 key, the repository password, a bearer token | a private key it made itself, the repository password |
| Agent to server | HTTPS, server pinned, bearer token | HTTPS, server pinned, the agent's pinned certificate (mutual TLS) |
| Backup data path | host to S3 | host to the server's gateway to S3 |
| Off-site copies | the vault pulls | the server copies |
| Off-site settings | in the vault's own UI | on the job, in the one UI |

## 2. Parts, in build order

**1. Agent certificates.**
- `holdfast-agent enroll` makes an ECDSA key and a self-signed certificate (root-only files), and sends the certificate
  with the one-time code. The server pins its fingerprint for that host.
- The agent listener asks for a client certificate. An agent route answers only to a pinned one; the host's identity comes
  from it, never from the request. `/metrics` and `/healthz` need none.
- The bearer token goes: from the agent, the store and the protocol (protocol 4). A new code and a new enrollment replace
  a certificate; removing or rotating a host unpins it.
- TLS on the agent listener is required. The e2e harness runs every test with it.

**2. The gateway.**
- restic's REST protocol under `/gateway/v1/<fqdn>/` on the agent listener: config, keys, snapshots, index, data, locks.
- A host reads and writes its own repository only. Deletes are refused except for lock files. Uploads and downloads are
  streamed, not buffered.
- It uses the gateway key: `storage.gateway_credentials_file` in `server.yaml`. New storage checks replace the host-key
  checks (B001–B007): the gateway key writes and reads under `repos/`, deletes only lock files, and cannot delete a
  version or use another prefix.
- The agent's restic gets `rest:https://…`, the pinned server certificate (`--cacert`) and the agent's certificate
  (`--tls-client-cert`), from files.
- Metrics per host (requests, bytes, refusals), and an audit entry for each refusal.
- An agent that cannot reach the gateway retries, and backs up as soon as it answers.

**3. Per-host S3 keys go.**
- `holdfast host add` without a key; `host set-key`, the provisioner hook and the credentials' S3 fields are removed.
- A restore onto another host needs no read-only key: the server allows that host's certificate to read the other
  repository through the gateway for that one restore.
- The escrow bundle holds passwords only.

**4. Off-site copies by the server. Optional.**
- **A site B is optional** (your instruction, 2026-11-22). Without `offsite:` in `server.yaml`, Holdfast runs with one
  bucket: nothing is copied, the API answers that off-site is not set up, the UI and the CLI show no off-site setting,
  column or action, the checks and the alerts about off-site stay silent, and the e2e suite's other tests all run
  without it. Adding `offsite:` later starts copying every job's snapshots from then on.
- `offsite:` in `server.yaml`: the endpoint, the bucket and its maintenance key. Tested with the same contract as the
  primary bucket.
- The copier (M7's) runs in the server: after a backup, and every few minutes, `restic copy` from the primary repository
  into the off-site one, made with the same chunker parameters, with a password of its own, escrowed there.
- Each job has an off-site retention (it starts as a copy of primary), Exclude and Match primary, in the job's own
  settings: UI, API and CLI. O101 warns when off-site keeps less. The daily run does forget, check and prune off-site too,
  behind the 7-day protection.
- The hold, "Deleted in primary" and the lag alert stay. A copy's snapshots are listed and browsed like any others.
- A restore from off-site runs on the host through the gateway, read-only, into a new or another directory.
- The vault role, the vault protocol, grants, restore keys, `holdfast vault`, `holdfast --vault`, the holdfast-vault
  package and rclone are removed.

**5. Documents and the lab.**
- spec, storage.md (two keys per bucket, their policies), install.md, the runbooks, CLAUDE.md's rules and table, and the
  "How Holdfast works" page.
- The lab: every agent enrolled again, backups and restores through the gateway on Ceph, off-site copies in a second
  bucket, a restore from off-site.

## 3. What does not change

- The repositories stay where they are, `repos/<fqdn>/` in the same bucket, so no data moves. Agents enroll again.
- The agent keeps its own schedule and its cached config. restic still encrypts on the host: the gateway sees only
  encrypted files.
- Config revisions, checks, retention decided by restic, the 7-day protection, Prune now, Delete now, rollback, escrow
  with age, dumps, the bounds a host sets on restores, roles and login.

## 4. Tests

- **Unit:** the gateway's rules for each restic file type and method; a certificate's host; path tricks (`..`, another
  host's name, encoded slashes).
- **e2e**, real restic through the real gateway on MinIO:
  - every existing test, on TLS with certificates;
  - host A's certificate on host B's repository is refused, and so is a delete, and both are audited;
  - an unpinned certificate gets nothing; a removed host's certificate stops working;
  - the server stopped during a backup: the run fails, is retried, and succeeds once the server is back;
  - a large backup and `restic check --read-data` through the gateway;
  - off-site: M7's `TestVault`, turned into `TestOffsite` with one server;
  - a restore onto another host, and from off-site, through the gateway.
- **Lab:** as §2 part 5, on Ubuntu, AlmaLinux and Oracle Linux with Ceph RGW; the fidelity test must still keep 39 of 39.

## 5. Order and size

Parts 1 to 5 in that order, each ending with the whole e2e suite green. Part 2 is the largest new piece; part 4 mostly
moves and removes code. About the size of M7.

## 6. What I chose inside the decision

Say so if you want any of these otherwise.

- **Holdfast's own gateway**, not the pinned `rclone serve restic` the review preferred as a first step. The protocol is
  small, and the gateway key cannot delete, which bounds what a bug in it can do.
- **One gateway, in the server.** A second one on another machine is designed for, not built.
- **Certificates pinned like SSH keys**, with no CA and no expiry to manage. The certificate's own dates are ignored: the
  pin is what counts.
- **Object Lock is reported, not required, and locks are not extended.**
- **M8 comes before the rebuild** (now M9, [plan-m9](plan-m9.md)), so the rebuild is designed once, on this
  architecture. Nothing is in production to lose meanwhile.
- **Second-admin approval** is left for a later milestone.
- **Parts 2 and 3 are built as one step.** The gateway is what makes the per-host keys go: their checks, the
  credentials and the restore keys change together, and an agent that used both would be tested for nothing.
- **The gateway never rewrites a file and never writes a repository's config or keys.** restic names its files by
  their content, so a file that exists already holds what a second upload holds; and only the server makes a
  repository or changes its keys. A restic key added outside Holdfast can then only come from someone with S3 access.
- **Off-site runs inside holdfast-server as a second instance of the same code**, the M7 copier with its retention,
  hold, Exclude, Match primary and "Deleted in primary", on its own PostgreSQL schema (`offsite`, in the same
  database) and its own bucket. The two talk in-process in place of the vault protocol: no registration, no token, no
  rclone, no restore keys. The copier reads the primary repository through a loopback read-only gateway, so restic
  needs only the off-site key. People reach it through the same UI, API and CLI: `/api/v1/offsite/…`,
  `holdfast --offsite …`, and the UI's Off-site pages, with the same login and the same bindings. Its config history
  and audit log are its own, as site B's were. This keeps M7's tested behaviour instead of rebuilding it on a site
  column in every table.
- **Agents enroll again** when they are upgraded to M8, in place of a migration path, since Holdfast has no production
  installs.
