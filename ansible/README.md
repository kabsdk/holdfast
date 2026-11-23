# kabsdk.holdfast

Holdfast's installer as Ansible roles. Holdfast runs restic backups for a fleet of Linux servers from one place: one
server, an agent on every host, and your own S3 bucket. This collection sets up the server, its agents and the
`holdfast` command, with the same settings as Holdfast's `install.sh`, and keeps them that way. A second run with
nothing to do changes nothing.

| Role | Group in the inventory | What it does |
| --- | --- | --- |
| `kabsdk.holdfast.server` | `holdfast_server` (one host) | Installs the server, its PostgreSQL, Holdfast's CA and the recovery file. Writes `server.yaml` from your settings and starts the server. |
| `kabsdk.holdfast.agent` | `holdfast_agents` | Installs the agent and restic. Adds the host on the server and enrolls the agent. Gives the host the jobs it lacks, and sets its dumps and restore limits. |
| `kabsdk.holdfast.cli` | `holdfast_cli` | Installs the `holdfast` command on a workstation. |

The playbook `kabsdk.holdfast.site` runs the three roles on those groups, server first.

## Install the collection

```
ansible-galaxy collection install https://github.com/kabsdk/holdfast/releases/download/v0.5.0/kabsdk-holdfast-0.5.0.tar.gz
```

It needs ansible-core 2.16 or later, plus `ansible.posix` and `community.general`, which it installs with it. The
machines need Python 3, systemd, x86_64, and apt or dnf: Ubuntu, Debian, RHEL, Oracle Linux and their relatives.

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
          system: --path /etc --path /home
      hosts:
        web01.example.com:
        db01.example.com:
          holdfast_dumps:
            postgres: {command: [/usr/bin/pg_dumpall], user: postgres}
          holdfast_jobs:
            system: --path /etc
            postgres: --dump postgres --daily 01:00
```

```
ansible-playbook -i inventory.yml kabsdk.holdfast.site
```

Keep the secrets in Ansible Vault. Pin the release with `holdfast_version: 0.5.0`; without it the roles install the
latest.

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

Settings that only the roles have:

| Variable | Role | What it does |
| --- | --- | --- |
| `holdfast_server_yaml` | server | Anything else `server.yaml` takes, merged over what the role writes, such as `{maintenance: {retention: "off"}}`. |
| `holdfast_firewall` | server | Opens the agents' and the UI's ports in firewalld or ufw when either is on. Default `true`. |
| `holdfast_server_host` | agent | The server's host in the inventory. Default: the first host in `holdfast_server`. |
| `holdfast_jobs` | agent | Jobs by name, each as the flags of `holdfast job set`. |
| `holdfast_dumps` | agent | The dumps a dump job may name, set in `agent.yaml`. |
| `holdfast_restore` | agent | The host's restore limits in `agent.yaml`, such as `{allow_in_place: true}`. |
| `holdfast_reenroll` | agent | Enrolls an enrolled host again. Give it once, with `-e`. |

## What the roles keep, and what they leave to Holdfast

- **`server.yaml` follows your settings.** The installer ignores settings once a server is set up. The server role
  writes `server.yaml` on every run, and a change restarts the server.
- **Some things are made once and never again.** These are the database's password, the credentials key, the recovery
  file's age key, Holdfast's CA and the certificates it signs. A new name for the server needs a new certificate:
  delete `/etc/holdfast/tls/server.crt` or `ui.crt` and run again. The agents pin the CA, so they keep working.
- **Each host is enrolled once.** The agent role enrolls a host that has no `/etc/holdfast/agent.yaml`. It asks the
  server for the code with `holdfast host add`, or with `holdfast host enroll-code` for a host Holdfast knows already.
  To enroll with a code you got yourself, give `holdfast_server`, `holdfast_code` and `holdfast_ca_sha256` as
  `holdfast host add` prints them. The code is used once and is then ignored.
- **The roles only make the jobs a host lacks.** A job the host already has is left as it is. After the first run,
  jobs belong to Holdfast's UI and CLI, with their checks and revisions, so a change made there is not undone on the
  next run.
- **Dumps and restore limits belong to the role.** Only root on the host can set them; the server cannot. When
  `holdfast_dumps` or `holdfast_restore` is set, the role writes it to `agent.yaml` and restarts the agent. When it is
  not set, the role leaves them as they are.
- **The roles remove nothing.** Removing a host is `holdfast host remove`, or **Remove host** in the UI. It never
  deletes data. Deleting a host's backups is an admin's decommission.

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
may stay in the inventory. Once `holdfast host list` shows the hosts back, delete `/root/holdfast-recovery.txt` on the
server.

## Mirrors

Hosts that cannot reach GitHub install from a mirror. A mirror is any HTTP(S) directory that holds one release's
`VERSION`, `SHA256SUMS` and packages, as published. Set `holdfast_download_url` to it on every machine. The server
writes the mirror into `server.yaml` as well, so the lines `holdfast host add` prints use it too.

## Testing

`make packages ansible-test`, in Holdfast's repository, builds and installs the collection as above and runs
ansible-lint with the production profile. It then runs the playbook on Ubuntu, AlmaLinux and Oracle Linux machines
against MinIO, twice, and checks that the second run changes nothing. It also replaces a lost server from the
recovery file.
