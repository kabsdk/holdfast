# Installing and upgrading

Holdfast comes as `.deb` and `.rpm` packages for amd64, built by `make packages` into `dist/`. They are not
signed yet (plan-m5 Q2): check them against `dist/SHA256SUMS` before you install them.

| Package | Holds | Needs |
| --- | --- | --- |
| `holdfast-restic` | restic, the version Holdfast is tested with, at `/usr/libexec/holdfast/restic` (decision 032) | |
| `holdfast-agent` | `/usr/bin/holdfast-agent` and `holdfast-agent.service` | `holdfast-restic` of the same version |
| `holdfast-server` | `/usr/bin/holdfast-server`, `holdfast-server.service`, the example `server.yaml`, the alert rules and scripts | `holdfast-restic` and `holdfast-cli` of the same version |
| `holdfast-cli` | `/usr/bin/holdfast` | |
| `holdfast-freshness` | `/usr/bin/holdfast-freshness`, the storage witness, and its unit | |
| `holdfast-vault` | `/usr/bin/holdfast-vault`, `holdfast-vault.service`, the example `vault.yaml`, and the pinned rclone at `/usr/libexec/holdfast/rclone` | `holdfast-restic` and `holdfast-cli` of the same version |

The binaries are static, so the same packages run on Ubuntu, Debian, RHEL, Oracle Linux and their
relatives. The lab installs them on Ubuntu, RHEL and Oracle Linux (`lab/README.md`).

## Installing

```
sha256sum -c --ignore-missing SHA256SUMS
apt install ./holdfast-restic_*.deb ./holdfast-agent_*.deb        # Debian, Ubuntu
dnf install ./holdfast-restic-*.rpm ./holdfast-agent-*.rpm        # RHEL, Oracle Linux
```

Installing enables nothing, because each service needs its config first:
- **The server:** make the bucket and its two keys, the gateway key and the maintenance key
  ([storage.md](storage.md)), and a TLS certificate for the agent listener, which is required. Write
  `/etc/holdfast/server.yaml`, using `/usr/share/holdfast/server.yaml.example`, with `ca_sha256` set to the sha256
  of your CA's certificate (or of the server's own, if it is self-signed), then `systemctl enable --now
  holdfast-server`. The alert rules and the alerting scripts are in `/usr/share/holdfast/` too
  ([alerting.md](alerting.md)).
- **An agent:** run the command `holdfast host add` printed, as root on the host:
  `holdfast-agent enroll --server … --fqdn … --code … --ca-sha256 …`. It makes the agent's own key and
  certificate, which the server pins; nothing else is needed on the host, no S3 key. Then `systemctl enable
  --now holdfast-agent`.
- **The witness:** see `holdfast-freshness.service`'s comments.
- **A vault**, in the other site (spec §3.7), with a PostgreSQL and a bucket of its own there:
  1. on site A's server, as root: `holdfast vault add site-b --s3-key-file <site A read key>` (docs/storage.md). It
     prints the vault's token once;
  2. on the vault's machine: put the token alone in a root-only file, run `holdfast-vault keygen
     /etc/holdfast-vault/credentials.key`, and write `/etc/holdfast-vault/vault.yaml` from
     `/usr/share/holdfast/vault.yaml.example`: `role: vault`, site B's bucket and maintenance key, and `source:` with
     site A's agent URL, its CA fingerprint and the token file;
  3. `systemctl enable --now holdfast-vault`. Its CLI is `holdfast --vault`, as root on that machine;
  4. for restores from site B, give each copied host its restore key: `holdfast --vault host set-key <fqdn>
     --s3-key-file <key>`.

  A vault may run on the same machine as its site's own holdfast-server; their ports, state and sockets differ.

The agent's unit runs it at low CPU and I/O priority (`Nice=10`, `IOSchedulingClass=idle`), so a backup
gives way to the host's own work.

## Upgrading

**From a release before M8, every agent enrolls again** (decision 041): it had a bearer token and an S3 key, and
is known by its own certificate from now on, and backs up through the server. Upgrade the server, give it its
gateway key and TLS, then for each host: upgrade the agent's package, `holdfast host enroll-code <fqdn>`, and run
the printed `holdfast-agent enroll …` on the host. Its repository and password stay. Delete the host's old S3 key
at your S3 server once it has backed up through the gateway.

**Otherwise, upgrade the server first, then the agents, in any order and at any pace.** Upgrade a vault after its
source's server. The agent sends its protocol
version with every poll, and the server answers with the range it accepts:
- a server accepts every agent from its `MinProtocol` on, so agents can be upgraded one by one, after it;
- an agent that is too old is refused with that range, and logs "the server needs a newer agent". It keeps
  backing up on its cached config, and the server still takes its reports. The host shows the
  `agent_too_old` problem and `holdfast_agent_too_old` is 1 until it is upgraded;
- an agent newer than its server logs "upgrade the server first".

Upgrading a package restarts its service if it runs. The server migrates its database when it starts. An
agent restarted in the middle of a backup reports that run as interrupted; the job's next run backs up as
usual. So upgrade agents outside their backup windows, or check `holdfast host events` after.

```
apt install ./holdfast-restic_*.deb ./holdfast-server_*.deb ./holdfast-cli_*.deb
dnf upgrade ./holdfast-restic-*.rpm ./holdfast-server-*.rpm ./holdfast-cli-*.rpm
```

Each release pins one restic, so `holdfast-restic` moves with the other packages. A host that runs another
restic gets the `restic` warning (decision 032).

## Removing

Removing a package stops and disables its service. It never deletes `/etc/holdfast`, the agent's state or
any backup. To stop backing up a host, remove it in Holdfast (`holdfast host remove`), and to delete its
repository, decommission it (spec §3.5).
