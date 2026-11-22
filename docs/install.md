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
- **Off-site copies**, optional (spec §3.7): make the off-site bucket and its key ([storage.md](storage.md)), add
  the `offsite:` section to `server.yaml` (the example has it, commented out), and restart holdfast-server. It makes
  the schema `offsite` in its own database and starts copying. Nothing else is installed or registered.

The agent's unit runs it at low CPU and I/O priority (`Nice=10`, `IOSchedulingClass=idle`), so a backup
gives way to the host's own work.

**Keep offline, from the start**, what a rebuilt server needs (decision 042: one server, replaced if lost;
[runbooks/rebuild-holdfast.md](runbooks/rebuild-holdfast.md)): the escrow's age key, the credentials key,
`server.yaml` with every file it names, and the agent listener's TLS certificate and key, or at least their CA, which
the agents pin. The server exports its database every night to `meta/` in the bucket, encrypted to the age key; a
new server is restored from the newest with `holdfast-server restore-export`.

## Upgrading

**From a release before M8, every agent enrolls again** (decision 041): it had a bearer token and an S3 key, and
is known by its own certificate from now on, and backs up through the server. Upgrade the server, give it its
gateway key and TLS, then for each host: upgrade the agent's package, `holdfast host enroll-code <fqdn>`, and run
the printed `holdfast-agent enroll …` on the host. Its repository and password stay. Delete the host's old S3 key
at your S3 server once it has backed up through the gateway. An M7 vault is not upgraded: remove its package, and
give the server `offsite:` with a new, empty bucket and a key for it; the copies start again from the snapshots the
primary has. The vault's old bucket keeps its copies, with the passwords in its own escrow files.

**From a release before M9**, give the maintenance key `s3:GetObject`, `s3:PutObject` and `s3:DeleteObject` on
`meta/*` in each bucket ([storage.md](storage.md)): the nightly export goes there, and B016 fails without them.

**Otherwise, upgrade the server first, then the agents, in any order and at any pace.** The agent sends its protocol
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
