# 044: A one-line install

Status: accepted, 2026-11-23. You asked for installation "in a zerobyte-way (curl some sh script, like k3s as well)",
and left it to me whether it should stop on missing settings.

## Decision

1. **`install.sh`, released beside the packages**, run as root on each machine:

   ```
   curl -sfL https://github.com/kabsdk/holdfast/releases/latest/download/install.sh | sh -s - server   # the server
   curl -sfL …/install.sh | HOLDFAST_SERVER=… HOLDFAST_CODE=… sh -                                    # an agent
   curl -sfL …/install.sh | sh -s - cli                                                               # a workstation
   curl -sfL …/install.sh | sh -s - witness                                                           # the storage witness
   ```

   The role is the first argument, and `agent` when there is none, since agents are the many. Settings are
   `HOLDFAST_*` environment variables. The URL is the GitHub release until the website exists.

2. **A missing or wrong setting stops it before it changes anything.** It checks every setting first and prints all
   that are missing or wrong, each with a line saying what it is, then exits 1. It never prompts: `curl | sh` has no
   terminal to ask on, and an install that guesses a storage key or a recipient is worse than one that stops.

3. **What it installs.** The packages of one release, checked against that release's `SHA256SUMS`, with apt or dnf.
   `HOLDFAST_VERSION` pins a release, and `HOLDFAST_DOWNLOAD_URL` names a mirror for machines that cannot reach GitHub.

4. **Run again, it upgrades.** On a machine that is set up already, it only upgrades the packages and says the
   settings are ignored; `server.yaml` is changed by hand. An agent given a new code enrolls again.

   **On a new machine with `/etc/holdfast` put back**, the server's line prepares a rebuild (decision 040): it installs
   PostgreSQL with the database password from the restored `db.dsn`, installs the packages, and leaves the server
   stopped, so `holdfast-server restore-export` loads the nightly export into an empty database before the server
   first starts and migrates it.

5. **The server's first install** does what `install.md` listed by hand:
   - the distribution's PostgreSQL, with a database and a user with a random password, unless
     `HOLDFAST_DATABASE_DSN` names one; on RHEL 9 and its relatives, the module stream 16, which the lab uses;
   - the credentials key (`holdfast-server keygen`);
   - for the agent listener, a CA of its own and a certificate it signs for the public URL's name, unless
     `HOLDFAST_TLS_CERT` and `HOLDFAST_TLS_KEY` are given; agents pin the CA, so the certificate can be replaced
     without enrolling them again;
   - `server.yaml`, with the storage, the escrow recipient and, if given, off-site copies and the web UI;
   - the ports in firewalld or ufw, if one runs; then it starts the server, waits for it, and prints the pin, what to
     copy offline, and the next step.

   Required: the S3 endpoint and bucket, the gateway key, the maintenance key, and **the escrow recipient**. Without
   a recipient there is no escrow and no nightly export, so a lost server could not be rebuilt (decisions 036, 040).
   Making the bucket and the keys stays with the storage admin (decision 029). The web UI needs an OIDC provider
   (decision 024), so its six settings are all or none; without them the CLI works as root on the server.

6. **`host add` prints the one line.** `host add`, `host enroll-code`, `host rotate` and `host import` answer with
   `install_script` beside `install_command`: the line with the server's URL, its pin, the host, the code, and the
   server's own version, or `download_url` from `server.yaml` for a mirror. UI = API = CLI: the UI shows the line,
   and the enrollment command alone for a host whose packages are installed already, such as by configuration
   management.

## What it costs

- `curl | sh` runs a script fetched over HTTPS as root. That is k3s's trust model too. The docs show how to download
  it, read it and run it instead, and the script checks every package against `SHA256SUMS`.
- Settings in a command line land in the shell's history. The docs put the server's secrets in a file that is sourced
  instead. An enrollment code in the history is harmless once used, or after 15 minutes.
- A second path to keep working. `make install-test` runs it on containers with systemd (Ubuntu 24.04, AlmaLinux 9,
  Oracle Linux 9) against MinIO: refused settings, the server's install, the agent's one line, a backup, upgrades,
  the witness and the CLI, and a lost server replaced from `/etc/holdfast`, the same line and the nightly export.
  systemd cannot hand the witness its key in a container's `/run`, so there the witness runs once with the settings
  the installer wrote; its unit was checked on the lab's AlmaLinux VM.

## Why

- **It is how the tools this audience knows install.** k3s, Zerobyte and others: one line, settings in the
  environment, run again to upgrade.
- **Stopping on a missing setting is safer than asking or guessing.** The storage keys and the escrow recipient
  decide whether backups can be deleted and whether a lost server can be rebuilt. They are given once, on purpose.
- **The packages stay the one thing that is installed.** The script only drives apt or dnf, so `install.md`'s manual
  path and configuration management install exactly the same files.

## What changes

- `scripts/install.sh`, in each release beside the packages and in `SHA256SUMS`.
- `server.yaml` gains `download_url`; enrollments gain `install_script`; the UI's enroll panels show it.
- `install.md` leads with the one line; spec §5.7 and §6.4; `make install-test`.
