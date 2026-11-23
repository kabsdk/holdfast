# 050: An Ansible collection, kabsdk.holdfast

Status: accepted, 2026-11-23. You asked for "an ansible collection for the server and agent. Must be idempotent, follow
best practises, be as simple as possible while retaining all functionality", and for it to be committed and released.

## Decision

1. **The collection lives in this repository**, in `ansible/`, as `kabsdk.holdfast`. It is released with Holdfast, under
   the same version, as `kabsdk-holdfast-<version>.tar.gz` beside the packages on the GitHub release (`make
   collection`). It is not on Ansible Galaxy yet: that needs a Galaxy namespace and its token.
2. **Three roles and a playbook.** `server`, `agent` and `cli` match the installer's three roles. A fourth, `packages`,
   is internal and shared by the other three. The playbook `site` runs them on the groups `holdfast_server`,
   `holdfast_agents` and `holdfast_cli`, server first.
3. **The settings are the installer's** (decision 044), in lower case. Three things change: files are paths on the
   controller, lists are YAML lists, and no UI is `holdfast_ui_url: false`. The roles check the same settings, with the
   same rules, before they change anything.
4. **The roles are clients, like the installer.** They install the same packages from the same release files, checked
   against `SHA256SUMS`, and run the same commands: `holdfast-server recovery-file`, `keygen` and `restore-export`,
   `holdfast-agent enroll`, and `holdfast host add`, `host enroll-code` and `job set` as root on the server. They add no
   API and no check of their own (decision 028).
5. **Who owns what:**
   - The roles own `server.yaml`. It follows the settings on every run, and a change restarts the server. The
     installer, by contrast, ignores settings once a server is set up.
   - Some things are made once and kept: the database's password, the credentials key, the recovery file's age key,
     Holdfast's CA and the certificates it signs.
   - A host is enrolled once, while it has no `agent.yaml`, unless `holdfast_reenroll` is given.
   - The roles make the jobs a host lacks, and never change or remove a job. After that, jobs belong to the UI and the
     CLI, with their checks and revisions.
   - The roles own the host-only parts of `agent.yaml`, the dumps and the restore limits, when they are given.
   - The roles remove nothing. Removing a host stays `holdfast host remove`, and deleting data stays decommission.
6. **Idempotent, and tested so.** A run with nothing to do changes nothing. `make ansible-test` builds and installs the
   collection, runs ansible-lint with the production profile, and runs the playbook against real machines (containers
   with systemd), MinIO and a mirror of the packages. It checks that the second run reports `changed=0` on every host,
   and that a lost server is replaced from the recovery file.

## What it costs

- There are now two ways to install Holdfast. A new installer setting needs a role variable and a line in the test as
  well. The two tests (`make install-test`, `make ansible-test`) run the same scenarios, so a gap shows up in one of
  them.
- ansible-lint's rule `var-naming[no-role-prefix]` is skipped. Each role's variables are named `holdfast_*`, after the
  installer's settings, rather than `server_*` in one role and `agent_*` in another. One name for one setting matters
  more here than the rule.

## Why

- **The installer's settings are already the interface.** The same names, checks and files mean the documentation,
  the runbooks and the rebuild work the same whichever way a server was installed. A server set up with `install.sh`
  can be taken over by the roles without a change.
- **Jobs are Holdfast's to manage.** Each job change is a revision, with checks, accepted codes and an audit entry. If
  Ansible kept jobs in step, it would undo the UI's changes on every run, and two places would claim the same config.
  Making only the missing jobs gives a new host its backups from the start and leaves the rest to Holdfast.
- **Dumps and restore limits are the host's.** Only root on the host may set them (decisions 030 and 037), and that is
  what configuration management is for.
