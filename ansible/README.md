# kabsdk.holdfast

Holdfast's installer as Ansible roles. Holdfast runs restic backups for a fleet of Linux servers from one place: one
server, an agent on every machine it backs up, and your own S3 bucket. This collection sets up the server, its agents
and the `holdfast` command, with the same settings as Holdfast's `install.sh`, and keeps them that way. A second run
with nothing to do changes nothing.

| Role | Group in the inventory | What it does |
| --- | --- | --- |
| `kabsdk.holdfast.server` | `holdfast_server` (one host) | Installs the server, its PostgreSQL, Holdfast's CA and the recovery file. Writes `server.yaml` from your settings and starts the server. |
| `kabsdk.holdfast.agent` | `holdfast_agents` | Installs the agent and restic. Adds the agent on the server and enrolls it. Gives it the jobs it lacks, and sets its restore limits and its PostgreSQL client. |
| `kabsdk.holdfast.cli` | `holdfast_cli` | Installs the `holdfast` command on a workstation. |

The playbook `kabsdk.holdfast.site` runs the three roles on those groups, server first. It gathers no facts: the
roles gather the little they need themselves, so it works with `gathering = explicit` in `ansible.cfg`.

## Install the collection

```
ansible-galaxy collection install https://github.com/kabsdk/holdfast/releases/download/v0.1-dev/kabsdk-holdfast-0.1-dev.tar.gz
```

0.1-dev is a development release, subject to change: until 0.1.0, newer builds replace it under the same name.

It needs ansible-core 2.16 or later, plus `ansible.posix` and `community.general`, which it installs with it. The
machines need Python 3, systemd, x86_64, and apt or dnf: Ubuntu, Debian, RHEL, Oracle Linux and their relatives.

It goes into the first directory of your `collections_path` (`ansible.cfg`), which is `~/.ansible/collections` unless
you set one; `-p <dir>` puts it elsewhere. A repository that pins its collections lists it in `requirements.yml`, and
installs them with `ansible-galaxy collection install -r requirements.yml`:

```yaml
collections:
  - name: https://github.com/kabsdk/holdfast/releases/download/v0.1-dev/kabsdk-holdfast-0.1-dev.tar.gz
    type: url
```

## Quick start

First make the bucket and its two keys, the gateway key and the maintenance key (see *The bucket and its keys* in
Holdfast's documentation). Then write an inventory:

```yaml
# inventory.yml
all:
  children:
    holdfast_server:
      hosts:
        holdfast.example.com:
          holdfast_fqdn: holdfast.example.com
          holdfast_s3_endpoint: https://s3.example.com
          holdfast_s3_bucket: holdfast
          holdfast_gateway_access_key_id: "{{ vault_gateway_key_id }}"
          holdfast_gateway_secret_access_key: "{{ vault_gateway_secret }}"
          holdfast_maintenance_access_key_id: "{{ vault_maintenance_key_id }}"
          holdfast_maintenance_secret_access_key: "{{ vault_maintenance_secret }}"
          holdfast_mail_to: [backup-team@example.com]
          holdfast_smtp: relay.example.com:587
          holdfast_heartbeat_url: https://hc-ping.com/…
    holdfast_agents:
      vars:
        holdfast_jobs:
          system: {path: [/etc, /home], exclude: ["*.tmp"]}
      hosts:
        web01.example.com:
          holdfast_agent_name: web01.example.com
        db01.example.com:
          holdfast_agent_name: db01
          holdfast_postgresql_bin_dir: /usr/lib/postgresql/18/bin
          holdfast_jobs:
            system: {path: [/etc]}
```

```
ansible-playbook -i inventory.yml kabsdk.holdfast.site
```

`holdfast_fqdn` is the server's DNS name: its URLs and certificates are made from it. `holdfast_agent_name` is an
agent's name in Holdfast, which you choose, such as `web01.example.com` or `db01`. Every agent needs one: the roles
read no name from the machine, and an enrolled agent keeps its name.

db01 has the PostgreSQL client, so it can back up PostgreSQL databases. Its connection to the database server is set
in Holdfast, not here (see [PostgreSQL](#postgresql) below).

Keep the secrets in Ansible Vault. Pin the release with `holdfast_version`, such as `0.1-dev`; without it the roles
install the latest. `0.1-dev` installs its newest build, which upgrades the one before.

After the first run:

- **Copy the recovery file offline.** The server role writes `/root/holdfast-recovery.txt` on the server. It holds the
  age key and the credentials key. Copy it to a safe place off the server, such as a password manager, then delete it
  there. Until then the server shows the problem `recovery_file`. The file opens every backup. With the two keys of the
  bucket it also rebuilds a lost server.
- **Log in to the web UI.** It is at `https://<the server>/`. As root on the server, `holdfast ui-login` prints a
  one-time link.

## Settings

Every setting of `install.sh` is a variable with the same name in lower case: `HOLDFAST_S3_BUCKET` is
`holdfast_s3_bucket`. `ansible-doc -t role kabsdk.holdfast.server` lists them all, and so do `…agent` and `…cli`. A few
take a form that suits Ansible better:

| Setting | In the installer | In the roles |
| --- | --- | --- |
| Certificates, keys, the recovery file | a path on the machine | a path on the Ansible controller (vault-encrypted files work) |
| `holdfast_mail_to`, `holdfast_admins` | comma-separated | a list |
| No web UI | `HOLDFAST_UI_URL=off` | `holdfast_ui_url: false` |
| The server's DNS name | `hostname -f` | `holdfast_fqdn`, required on the server role |
| An agent's name | `HOLDFAST_AGENT_NAME`, required | `holdfast_agent_name`, required on the agent role |

`holdfast_fqdn` is required because the roles read no name from the machine. `holdfast_public_url`
(`https://<holdfast_fqdn>:8443`), `holdfast_ui_url` (`https://<holdfast_fqdn>`), `holdfast_mail_from`
(`holdfast@<holdfast_fqdn>`) and the names in Holdfast's certificates default to it.

Settings that only the roles have:

| Variable | Role | What it does |
| --- | --- | --- |
| `holdfast_server_yaml` | server | Anything else `server.yaml` takes, merged over what the role writes, such as `{maintenance: {retention: "off"}}`. |
| `holdfast_firewall` | server | Opens the agents' and the UI's ports in firewalld or ufw when either is on. Default `true`. |
| `holdfast_server_host` | agent | The server's host in the inventory. Default: the first host in `holdfast_server`. |
| `holdfast_jobs` | agent | Jobs by name, each a mapping of the flags of `holdfast job set` (below). |
| `holdfast_restore` | agent | The agent's restore limits in `agent.yaml`, such as `{allow_in_place: true}`. |
| `holdfast_postgresql_bin_dir` | agent | The directory with `pg_dump`, `pg_dumpall`, `pg_restore` and `psql`, written to `agent.yaml`. Without it the agent takes no PostgreSQL job. Default empty, which leaves `agent.yaml` as it is. |
| `holdfast_postgresql_allow_in_place_restore` | agent | `true` lets an admin restore a dump over the database it came from. Default `false`. |
| `holdfast_reenroll` | agent | Enrolls an enrolled agent again. Give it once, with `-e`. |

**A job in `holdfast_jobs`** is a mapping of `holdfast job set`'s flags, with an underscore for a dash. A list repeats
the flag, `true` gives a flag alone, and `false` leaves it out:

```yaml
holdfast_jobs:
  etc: {path: [/etc, /home], exclude: ["*.tmp"], daily: "02:00", keep_daily: 14d, keep_monthly: 6mo}
  logs: {path: [/var/log], every: 4h, from: "01:00", no_spread: true, limit_upload: 20480}
```

The keys are `path`, `exclude`, `connection`, `database`, `globals`, `daily`, `every`, `from`, `weekly`, `at`,
`spread`, `no_spread`, `zone`, `keep_within`, `keep_hourly`, `keep_daily`, `keep_weekly`, `keep_monthly`,
`keep_yearly`, `keep_last`, `accept` and `limit_upload`, as Holdfast's documentation describes the flags. Quote times,
`"02:00"`: unquoted, YAML reads them as numbers.

## PostgreSQL

An agent with `holdfast_postgresql_bin_dir` backs up PostgreSQL databases with `pg_dump`, one job a database. Its
`pg_dump` must be at least as new as the database server. The connection, with the backup user's password, is set in
Holdfast, where it is sealed, and never in the inventory or `agent.yaml`:

```
holdfast agent connection set db01 main \
  --url "postgresql://db01.example.com:5432/?sslmode=verify-full&sslrootcert=/etc/holdfast/pg/ca.crt" \
  --user holdfast_backup --password-file /root/holdfast_backup.pw -m "db01's PostgreSQL"
holdfast agent connection test db01 main
```

Then give db01 its database jobs, in the inventory for the next run or with `holdfast job set` now:

```yaml
holdfast_jobs:
  system: {path: [/etc]}
  app: {connection: main, database: app, every: 6h}
  roles: {connection: main, globals: true, daily: "02:00"}
```

Set the connection before the run that makes the job: `holdfast job set` refuses a job whose connection the agent does
not have. Restoring a
dump over its own database is off unless `holdfast_postgresql_allow_in_place_restore` is `true`. A database in
Kubernetes is backed up by the agent's container image, not by this collection: see *PostgreSQL* in Holdfast's
documentation.

## What the roles keep, and what they leave to Holdfast

- **`server.yaml` follows your settings.** The installer ignores settings once a server is set up. The server role
  writes `server.yaml` on every run, and a change restarts the server.
- **Some things are made once and never again.** These are the database's password, the credentials key, the recovery
  file's age key, Holdfast's CA and the certificates it signs. A new name for the server needs a new certificate:
  delete `/etc/holdfast/tls/server.crt` or `ui.crt` and run again. The agents pin the CA, so they keep working.
- **Each agent is enrolled once.** The agent role enrolls a machine that has no `/etc/holdfast/agent.yaml`. It asks
  the server for the code with `holdfast agent add`, or with `holdfast agent enroll-code` for an agent Holdfast knows
  already. To enroll with a code you got yourself, give `holdfast_server`, `holdfast_code` and `holdfast_ca_sha256` as
  `holdfast agent add` prints them. The code is used once and is then ignored.
- **An agent keeps its name.** The role stops when `holdfast_agent_name` is not the name in `agent.yaml`: the name is
  the agent's identity and its repository's, and renaming one is not designed. An `agent.yaml` from before 0.1-dev's
  agents has the name as `fqdn:`; the role writes it as `agent_name:`.
- **The roles only make the jobs an agent lacks.** A job the agent already has is left as it is. After the first run,
  jobs belong to Holdfast's UI and CLI, with their checks and revisions, so a change made there is not undone on the
  next run.
- **Restore limits and the PostgreSQL client belong to the role.** Only root on the machine can set them; the server
  cannot. When `holdfast_restore` or `holdfast_postgresql_bin_dir` is set, the role writes it to `agent.yaml` and
  restarts the agent. When it is not set, the role leaves `agent.yaml` as it is.
- **The roles remove nothing.** Removing an agent is `holdfast agent remove`, or **Remove agent** in the UI. It never
  deletes data. Deleting an agent's backups is an admin's decommission.

## Upgrading

Set `holdfast_version` to the new release and run the playbook. It upgrades the server first, then the agents. Each
host downloads only when its packages are of another release, and checks every package against the release's
`SHA256SUMS`. The roles do not go back to an older release.

## A lost server

On a new machine with the same name, run the playbook with the same inventory and the recovery file:

```
ansible-playbook -i inventory.yml kabsdk.holdfast.site -e holdfast_recovery_file=/secure/holdfast-recovery.txt
```

The server role restores the bucket's newest nightly export. That brings back the database, the agents' certificate
and the CA, and the agents reconnect by themselves. The role uses the file only while the server is not set up, so it
may stay in the inventory. Once `holdfast agent list` shows the agents back, delete `/root/holdfast-recovery.txt` on
the server.

## Mirrors

Hosts that cannot reach GitHub install from a mirror. A mirror is any HTTP(S) directory that holds one release's
`VERSION`, `SHA256SUMS` and packages, as published. Set `holdfast_download_url` to it on every machine. The server
writes the mirror into `server.yaml` as well, so the lines `holdfast agent add` prints use it too.

## Testing

`make packages ansible-test`, in Holdfast's repository, builds and installs the collection as above and runs
ansible-lint with the production profile. It then runs the playbook on Ubuntu, AlmaLinux and Oracle Linux machines
against MinIO, twice, and checks that the second run changes nothing. It also replaces a lost server from the
recovery file.
