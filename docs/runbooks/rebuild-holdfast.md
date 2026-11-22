# Runbook: Holdfast's server rebuilt

Use this when the Holdfast server is lost: its machine, or its PostgreSQL. Holdfast runs as one server with no
standby (decision 042), so the server is replaced, not failed over: a new server, given the same bucket, picks up from
the newest nightly export (decision 040).

Nothing in the bucket is touched by the loss. While there is no server, agents keep their schedule but cannot back up,
since their backups go through the server's gateway; they back up as soon as the new server answers. The storage
witness, which lists the bucket directly, alerts if that takes longer than a job's stale threshold.

**What is lost:** everything after the newest export: config revisions, history and audit entries, and hosts added
since. Restores in flight end, and everyone logs in again. The steps below show what to make again.

## Keep these offline, before you need them

- **The escrow's age key** (`escrow-key.txt`), whose recipient is `escrow.age_recipient`. It decrypts the exports and
  each host's escrow file. Without it there is no export: the server writes none without a recipient.
- **The server's credentials key**, `credentials_key_file`, made by `holdfast-server keygen`. The export holds every
  repository password sealed with it, so the export alone opens no repository.
- **`server.yaml`, and every file it names**: the bucket's two keys (`maintenance.env`, `gateway.env`), the off-site
  key with `offsite:`, the database's DSN, the OIDC client secret, and the agent listener's TLS certificate and key.
  The export does not hold `server.yaml`, as it may hold the database's password. **Keep the TLS certificate's CA**:
  each agent pins `ca_sha256`, and refuses a server whose certificate comes from another CA.
- The packages, at the version the server ran or newer.

The exports are in the bucket under `meta/<date>.age`, one a night at 00:30 in the default zone; the newest 30 are kept.
With `offsite:`, each is in the off-site bucket too. `holdfast export list` shows them, and `holdfast export now`
writes one at once. Standard tools open one without Holdfast: `age -d -i escrow-key.txt 2026-11-22.age | tar -xz` gives
`manifest.json` and each table as CSV.

## The steps

1. **A machine with PostgreSQL** and an empty database and user for Holdfast, as `docs/install.md` makes them.
2. **The packages**: `holdfast-restic`, `holdfast-cli`, `holdfast-server` (and `holdfast-freshness` if it ran there).
   Do not start `holdfast-server` yet.
3. **Put `/etc/holdfast` back**: `server.yaml`, the credentials key and every file `server.yaml` names, root-only.
   If the new machine has another address, point `public_url`'s name at it: agents use that name.
4. **Restore the newest export**, as root:

   ```
   holdfast-server restore-export -identity escrow-key.txt -latest
   ```

   `-latest` reads the newest export from the bucket with the maintenance key. To restore another one, or one from
   the off-site bucket when the primary bucket is gone too, download it (with that bucket's maintenance key) and give
   its file instead of `-latest`. It checks, before it touches the database, that:
   - the age key opens the export, and the export is whole;
   - the export is not from a newer Holdfast than the one installed;
   - the credentials key opens the export's repository passwords;
   - the database holds no Holdfast table. It never loads into one that does.

   It then loads every table, the off-site ones too, migrates them to the installed version, and audits the restore.
   It prints the head revision and the number of hosts.
5. **Start the server**: `systemctl enable --now holdfast-server`.
6. **Check**:
   - `holdfast status` shows no problem, and `holdfast export list` says the server was restored, from which export;
   - `holdfast host list`: every agent enrolled before the export comes back online within a minute or two, by
     itself: the export holds each one's pinned certificate. Its snapshots are listed again;
   - a host that shows **"ahead"** ran a change made after the export. Its agent keeps that config, and the server
     sends it nothing, until you make the change again (`holdfast job set …`). A host with no such change needs nothing.
7. **Hosts added after the export** are not in it: their agents are refused. For each one:
   1. Decrypt its escrow file. The storage admin reads `escrow/<fqdn>.age` from the bucket with their own key (the
      maintenance key cannot read it): `age -d -i escrow-key.txt <fqdn>.age > escrow.json`.
   2. `holdfast host import <fqdn> --escrow-file escrow.json`. It checks the file is that host's and names this
      server's repository for it, tests the storage, and proves the password opens the repository. The UI has the
      same form, beside Add host.
   3. Run the enrollment command it prints on the host, as root. The agent makes a new certificate.
   4. Set its jobs again: the import adds the host without any.
   5. Delete `escrow.json`.
8. **The next export** is written by the new server at 00:30, or at once with `holdfast export now`.

## Walked in the lab

`lab/lab test m1` walks this runbook on the lab's server (`lab/test-rebuild.sh`, plan-m9 Q8): the server's database
dropped, `/etc/holdfast` and its state removed, the packages removed and installed again, `/etc/holdfast` put back from
the lab's state, the export restored, and the three agents back by themselves. `lab/README.md` has its results.
