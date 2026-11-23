# Runbook: Holdfast's server rebuilt

Use this when the Holdfast server is lost: its machine, or its PostgreSQL. Holdfast runs as one server with no
standby (decision 042), so the server is replaced, not failed over: a new server, given the same bucket, picks up from
the newest nightly export (decisions 040, 047).

Nothing in the bucket is touched by the loss. While there is no server, agents keep their schedule but cannot back up,
since their backups go through the server's gateway; they back up as soon as the new server answers. A lost server
sends no alert: its heartbeat stops, and the heartbeat service outside the site raises the alarm (with Prometheus,
`HoldfastServerDown` pages).

**What is lost:** everything after the newest export: config revisions, history and audit entries, and the jobs of
hosts added since. Restores in flight end, and everyone logs in again. The steps below show what to make again.

## Keep these offline, before you need them

- **The recovery file**, `/root/holdfast-recovery.txt` as the installer wrote it: the age private key and the
  credentials key. The age key opens the exports and each host's escrow file; the credentials key opens the export's
  repository passwords, so the export alone opens no repository. A server installed with your own age key has a file
  that holds the credentials key, to which you added your key's `AGE-SECRET-KEY-…` line; `holdfast-server
  recovery-file -config /etc/holdfast/server.yaml <path>` writes it for a running server.
- **The settings you installed with**: the bucket's two keys among them, and the OIDC client secret and the SMTP
  password if you gave them. New keys with the same policies work as well.
- The packages, at the version the server ran or newer: the newest release is never older than the export.

Everything else comes from the export: the database, the agent listener's certificate and key, the CA and its key,
which the agents pin, and the UI's certificate. The exports are in the bucket under `meta/<date>.age`, one a night at
00:30 in the default zone; the newest 30 are kept. With `offsite:`, each is in the off-site bucket too. `holdfast
export list` shows them, and `holdfast export now` writes one at once. Standard tools open one without Holdfast. age
refuses the credentials key's line, so give it the age key alone: `grep '^AGE-SECRET-KEY-' holdfast-recovery.txt >
age.key`, then `age -d -i age.key 2026-11-22.age | tar -xz` gives `manifest.json`, each table as CSV, and the files.

## The steps

1. **On the new machine, give it the lost server's name**: if it has another address, point `public_url`'s name at it,
   as agents use that name.
2. **Copy the recovery file to it**, root-only, and **run the server's install line with the settings of the first
   install and `HOLDFAST_RECOVERY_FILE`** (decision 044):

   ```
   set -a; . ./holdfast.env; set +a
   export HOLDFAST_RECOVERY_FILE=/root/holdfast-recovery.txt
   curl -sfL https://github.com/kabsdk/holdfast/releases/latest/download/install.sh | sh -s - server
   ```

   It installs PostgreSQL and the packages, writes `server.yaml` from the settings, and runs `holdfast-server
   restore-export -identity <recovery file> -latest`, which reads the newest export from the bucket with the
   maintenance key. Before it touches the database, that checks that:
   - the age key opens the export, and the export is whole;
   - the export is not from a newer Holdfast than the one installed;
   - the credentials key opens the export's repository passwords;
   - the database holds no Holdfast table. It never loads into one that does.

   It then loads every table, the off-site ones too, migrates them to the installed version, and audits the restore;
   writes the credentials key from the recovery file, and the TLS files from the export; and brings back each host
   that has an escrow file in the bucket but is not in the export (below). The installer then starts the server.
   It prints the head revision and the number of hosts.
3. **Check**:
   - `holdfast status`: `recovery_file` is expected until step 5; `holdfast export list` says the server was restored,
     from which export;
   - `holdfast host list`: every agent enrolled before the export comes back online within a minute or two, by
     itself: the export holds each one's pinned certificate, and the CA they pin. Its snapshots are listed again;
   - a host that shows **"ahead"** ran a change made after the export. Its agent keeps that config, and the server
     sends it nothing, until you make the change again (`holdfast job set …`). A host with no such change needs nothing.
4. **Hosts added after the export** came back from their escrow files, with their repositories and passwords and no
   job; `restore-export` names each one. For each: `holdfast host enroll-code <fqdn>`, run the line it prints on the
   host as root (the agent makes a new certificate), and set its jobs again with `holdfast job set`.
5. **Delete the recovery file from the server** once hosts are back; the `recovery_file` problem then ends.
6. **The next export** is written by the new server at 00:30, or at once with `holdfast export now`.

**Another export**, or one from the off-site bucket when the primary bucket is gone too, is restored by hand (the
installer's line always takes the newest). Do not run the server's line without `HOLDFAST_RECOVERY_FILE` first: it
would make a new CA and credentials key, which the export's do not match.
1. Install PostgreSQL with an empty database and user, and `holdfast-restic`, `holdfast-cli` and `holdfast-server` at
   the version the lost server ran or newer.
2. Write `/etc/holdfast/server.yaml` and the key files it names from your settings, with the TLS paths of the lost
   server and no credentials key file. The export's own settings are in it, as `server.yaml`, to copy from.
3. Download the export with that bucket's maintenance key, and run `holdfast-server restore-export -identity
   holdfast-recovery.txt <file>`: it writes the credentials key and the TLS files. Then `systemctl enable --now
   holdfast-server`, and steps 3 to 6.

**With a copy of `/etc/holdfast` instead** (the M9 way, which still works): put it back on the new machine, root-only,
and run the server's install line with no settings. With `/etc/holdfast` back and no Holdfast installed, it prepares a
rebuild: it installs PostgreSQL with the database password from `db.dsn` (when `db.dsn` names the local database the
installer made) and the packages, and leaves the server stopped. Then `holdfast-server restore-export -identity
holdfast-recovery.txt -latest` (an age key file alone works too, as the credentials key is in place), and
`systemctl enable --now holdfast-server`. The export's files are written only where none is there already.

`make install-test` walks both on containers of each distribution: the recovery file and the settings on a new
machine, and `/etc/holdfast` put back with `restore-export`; each time the agent comes back by itself and backs up.

## Walked in the lab

`lab/lab test m1` walks the `/etc/holdfast` path on the lab's server (`lab/test-rebuild.sh`): the
server's database dropped, `/etc/holdfast` and its state removed, the packages removed and installed again,
`/etc/holdfast` put back from the lab's state, the export restored, and the three agents back by themselves.
`lab/README.md` has its results.
