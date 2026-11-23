# Installing and upgrading

Holdfast comes as `.deb` and `.rpm` packages for amd64, built by `make packages` into `dist/`. They are not
signed yet: check them against `dist/SHA256SUMS` before you install them.

| Package | Holds | Needs |
| --- | --- | --- |
| `holdfast-restic` | restic, the version Holdfast is tested with, at `/usr/libexec/holdfast/restic` (decision 032) | |
| `holdfast-agent` | `/usr/bin/holdfast-agent` and `holdfast-agent.service` | `holdfast-restic` of the same version |
| `holdfast-server` | `/usr/bin/holdfast-server`, `holdfast-server.service`, the example `server.yaml`, the alert rules and scripts | `holdfast-restic` and `holdfast-cli` of the same version |
| `holdfast-cli` | `/usr/bin/holdfast` | |

M1's `holdfast-freshness`, the storage witness, left in M10: the server is the witness now (decision 045), and its
package replaces the old one.

The binaries are static, so the same packages run on Ubuntu, Debian, RHEL, Oracle Linux and their
relatives. The lab installs them on Ubuntu, RHEL and Oracle Linux (`lab/README.md`).

Holdfast is under Apache-2.0, and restic under BSD-2-Clause (decision 043). Each package keeps its licence, and the
licences of the software it contains, in `/usr/share/doc/<package>/`.

## Installing with one line

`install.sh` (decision 044) installs a release's packages, checked against its `SHA256SUMS`, and sets the machine up.
Run it as root. It checks every setting before it changes anything, and stops with the list of what is missing or
wrong. Run again, it upgrades. `sh install.sh help` lists every setting.

**The server.** First make the bucket and its two keys ([storage.md](storage.md)). Put the settings in a root-only
file, so the secrets stay out of the shell's history, and keep a copy offline: a rebuild needs them again.

```
# holdfast.env: required
HOLDFAST_S3_ENDPOINT=https://s3.example.internal
HOLDFAST_S3_BUCKET=holdfast
HOLDFAST_GATEWAY_ACCESS_KEY_ID=…
HOLDFAST_GATEWAY_SECRET_ACCESS_KEY=…
HOLDFAST_MAINTENANCE_ACCESS_KEY_ID=…
HOLDFAST_MAINTENANCE_SECRET_ACCESS_KEY=…
# alerts (decision 046): mail, a webhook, or both, and a heartbeat for a dead man's switch outside the site
HOLDFAST_MAIL_TO=backup-team@example.internal
HOLDFAST_SMTP=relay.example.internal:587
HOLDFAST_HEARTBEAT_URL=https://hc.example.org/ping/…
```

```
set -a; . ./holdfast.env; set +a
curl -sfL https://github.com/kabsdk/holdfast/releases/latest/download/install.sh | sh -s - server
```

It installs PostgreSQL from the distribution (or uses `HOLDFAST_DATABASE_DSN`), makes the credentials key, an age key
pair, and a CA that signs the agent listener's certificate and the web UI's, writes `server.yaml`, starts the server
and prints the pin agents use. Then:
- **The recovery file**, `/root/holdfast-recovery.txt` (decision 047): the age private key and the credentials key.
  Copy it to a safe place off the server, such as a password manager, then delete it there; until then the server
  shows the problem `recovery_file`. With the bucket's two keys it rebuilds a lost server, and with the escrow files it
  opens every repository. To use an age key of your own instead, give its public key as `HOLDFAST_ESCROW_RECIPIENT`:
  the file then holds the credentials key alone, and you add your key's `AGE-SECRET-KEY-…` line to it.
- **The web UI** is at `https://<the server's FQDN>/` (decision 049). Log in as root on the server with `holdfast
  ui-login`, which prints a one-time link. Browsers warn about the certificate until they trust
  `/etc/holdfast/tls/ca.crt`; give `HOLDFAST_UI_TLS_CERT` and `HOLDFAST_UI_TLS_KEY` for one they trust, or
  `HOLDFAST_UI_URL=off` for no UI. For others to log in as themselves, with roles from their groups, give the OIDC
  provider: `HOLDFAST_OIDC_ISSUER`, `HOLDFAST_OIDC_CLIENT_ID` and `HOLDFAST_OIDC_CLIENT_SECRET`, all three. An LDAP or
  Active Directory goes behind the provider (Authelia, Dex, Keycloak).
- **Hosts.** `holdfast host add web01.example.com` prints the line to run on the host. `holdfast job set
  web01.example.com system --path /etc --path /home` gives it a job, with the default schedule and retention (daily at
  02:00; 14 days, 8 weeks, 12 months, 3 years) unless you give others (decision 048).

`sh install.sh help` lists every setting, with off-site copies and the rest.

**An agent.** `holdfast host add` prints the line to run on the host, as root:

```
curl -sfL …/install.sh | HOLDFAST_SERVER=https://holdfast.example.internal:8443 HOLDFAST_FQDN=web01.example.com HOLDFAST_CODE=… HOLDFAST_CA_SHA256=… HOLDFAST_VERSION=… sh -
```

**A workstation's CLI**: `… | sh -s - cli`, then `holdfast login --server <the UI's URL>`, which needs the OIDC
provider.

To read the script before running it: `curl -sfLO …/install.sh`, read it, then `sh install.sh server`.

## Installing with Ansible

The collection `kabsdk.holdfast` (decision 050), released beside the packages, does what `install.sh` does, as the
roles `server`, `agent` and `cli`. It takes the same settings, in lower case: `HOLDFAST_S3_BUCKET` is
`holdfast_s3_bucket`. The roles keep the machines that way: a run with nothing to do changes nothing, and a changed
setting reaches `server.yaml`. The agent role also adds each host on the server, enrolls it, and gives it the jobs it
lacks. Unlike the installer, the roles read no host name from the machine: give the server and every host its
FQDN as `holdfast_fqdn`. They need no gathered facts.

```
ansible-galaxy collection install https://github.com/kabsdk/holdfast/releases/download/v0.5.0/kabsdk-holdfast-0.5.0.tar.gz
ansible-playbook -i inventory.yml kabsdk.holdfast.site
```

[../ansible/README.md](../ansible/README.md) has an inventory to start from, and explains what the roles keep and what
they leave to Holdfast.

## Installing by hand

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
  holdfast-server`. `holdfast-server recovery-file -config /etc/holdfast/server.yaml holdfast-recovery.txt` writes
  its recovery file; add your age key's line to it (decision 047). The Prometheus rules and scripts, optional, are in
  `/usr/share/holdfast/` ([alerting.md](alerting.md)).
- **An agent:** run the command `holdfast host add` printed, as root on the host:
  `holdfast-agent enroll --server … --fqdn … --code … --ca-sha256 …`. It makes the agent's own key and
  certificate, which the server pins; nothing else is needed on the host, no S3 key. Then `systemctl enable
  --now holdfast-agent`.
- **Off-site copies**, optional (spec §3.7): make the off-site bucket and its key ([storage.md](storage.md)), add
  the `offsite:` section to `server.yaml` (the example has it, commented out), and restart holdfast-server. It makes
  the schema `offsite` in its own database and starts copying. Nothing else is installed or registered.

The agent's unit runs it at low CPU and I/O priority (`Nice=10`, `IOSchedulingClass=idle`), so a backup
gives way to the host's own work.

**Keep offline, from the start**, what a rebuilt server needs (decision 042: one server, replaced if lost;
[runbooks/rebuild-holdfast.md](runbooks/rebuild-holdfast.md)): the recovery file and the settings you installed with,
the bucket's two keys among them. The server exports its database and its TLS files every night to `meta/` in the
bucket, encrypted to the age key; on a new machine, the server's line with `HOLDFAST_RECOVERY_FILE` restores the
newest and starts the server.

## Sizing

There is one server (decision 042), and every backup and restore passes through its gateway, so its network is what
a fleet shares. In the lab (`lab/lab sizing m1`), a server with 2 vCPUs and 2 GiB carried **66 MiB/s**: three agents
backed up 256 MiB each of new random data at once, 768 MiB in 11.6 s, with the server's CPU about a quarter busy. The
lab is one machine's KVM, Ceph and virtual network, so this is a floor, not a limit: on real hardware the server's
network link and the storage are what bound it. A fleet whose nightly changes take longer to cross that link than its
backup window needs a second, independent Holdfast with its own bucket; Holdfast does not spread one fleet over
several servers.

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

**From a release before M10:**
- give the maintenance key `s3:GetObject` on `escrow/*` too: a rebuilt server reads the escrow files of hosts added
  after the export, and B012 fails without it;
- remove `holdfast-freshness` where it runs, its list-only key, and its scrape job; the server reads the bucket itself
  (decision 045);
- write the recovery file, `holdfast-server recovery-file -config /etc/holdfast/server.yaml holdfast-recovery.txt`, add
  your age key's `AGE-SECRET-KEY-…` line to it, and keep it offline (decision 047). `holdfast escrow export` is gone;
- scripts that ran `host add` with a job's flags run `holdfast job set` after it (decision 048);
- for alerts without Prometheus, add `notify:` to `server.yaml` (decision 046). For the web UI without an OIDC
  provider, set `ui:` and log in with `holdfast ui-login` (decision 049). An upgrade does not change `server.yaml`.

**From 0.5.0**, dump jobs are gone (decision 052): Holdfast runs no command on a host.
- Before upgrading, have each host that had a dump write the export itself, with a systemd timer, as *Databases and
  other exports* in the documentation shows.
- After upgrading the server, give each dump job paths, such as `holdfast job set db01.example.com pg --path
  /var/backups/postgresql --exclude '*.tmp'`, or remove it. Until no dump job is left, the server refuses to save the
  config, with that reason. An agent of 0.6.0 fails a dump job's runs; one still at 0.5.0 runs it until it is upgraded.
- Take `dumps:` out of each host's `/etc/holdfast/agent.yaml`. The agent ignores it, and logs so, until you do.
- With Ansible, give the server and every host `holdfast_fqdn`, write each job in `holdfast_jobs` as a mapping of
  `holdfast job set`'s flags, and drop `holdfast_dumps` (`ansible/README.md`).

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
