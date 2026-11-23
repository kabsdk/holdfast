# Plan for M10: simpler to run

Status: accepted and built, 2026-11-23. You answered "Go ahead with what you think is the best choice": every proposal
in §6, and section 2 as it stands. The decisions are 045 (witness), 046 (alerts), 047 (recovery file, items 3 and 5),
048 (host add and job defaults) and 049 (the UI without a provider). Two changes while building: the witness compares
the snapshot the agent reported with the bucket by ID (problem `not_in_bucket`, rule `HoldfastBackupNotInBucket`),
not by time, as a snapshot's time is when the backup started and a long backup would have paged; and a new job that
names a schedule takes it whole, without the default's 20-minute spread, which would fail S001 on short intervals.

You found Holdfast "still a VERY complicated product" after reading the documentation, and
asked for an independent review of what could be simpler without giving up security, safety or restore. The reviewer
read the docs, the spec, the decisions and the code, and answered yes: roughly a third less to install and to recover a
lost server, a little less day to day. This plan takes its proposals 1 to 6, which I agree with, and leaves out 7.

Nothing here changes what protects the data: hosts hold no S3 key; the two S3 keys and what each may do; versioning and
the lifecycle rule as the floor no Holdfast key can lower; the 7-day protection; the restore limits each host sets;
the audit log; restore with plain restic when Holdfast is gone.

## 1. What M10 holds

| # | Change | The operator no longer | What it costs |
| --- | --- | --- | --- |
| 1 | **The server is the storage witness** (replaces decision 008). It exports each host's and each job's newest snapshot in the bucket from its catalogue, which it reads after every backup with restic and the maintenance key, straight from S3, not through the gateway. | installs `holdfast-freshness`, makes a third, list-only key and its policy, runs it on a second machine, scrapes it | A server bug that both invents a success and misreads the catalogue would go unseen. The witness never protected against a hacked server: its keys can write a fresh-looking snapshot file, which is all the witness reads. Since decision 041 nothing backs up while the server is down, and `HoldfastServerDown` pages then. |
| 2 | **The server sends its own alerts.** Mail through an SMTP relay, a webhook, or both, once when a problem starts and once when it ends, from the Problems it already computes; and a heartbeat URL it calls after each completed check cycle, for a dead man's switch outside the site. Prometheus metrics and rules stay for teams that have Prometheus. | runs Prometheus, its rules, a scrape config with a CA copy, the mail script with two units, jq, sendmail and the dead man's script, just to be told | The server judges itself, so a dead or hung server is caught only by the heartbeat service, which is the job today's required dead man's switch does already. |
| 3 | **One recovery file.** Without `HOLDFAST_ESCROW_RECIPIENT`, the installer makes the age key pair itself and writes `/root/holdfast-recovery.txt`: the age private key and the credentials key. The server shows the problem `recovery_file` until that file is gone from the server. The nightly export also carries the agents' CA and `server.yaml` without its secrets. A rebuild is the install line with `HOLDFAST_RECOVERY_FILE` and the two S3 keys (your original settings file, or new keys). | runs `age-keygen` before installing; keeps a copy of `/etc/holdfast` offline and current, with plaintext S3 keys and the database's password in it; puts it back with the right modes | The key is born on the server, not offline. Whoever controls the server at install time already has every password, so this gives them nothing new; the problem stands in for "offline from minute zero". The credentials key stays out of the bucket: age encrypts but does not authenticate, and the credentials key opening the export's passwords is what proves an export was the server's, not forged with the maintenance key. Your own age key is still accepted. |
| 4 | **`host add` adds a host; `job set` adds jobs, with defaults.** `holdfast host add <fqdn>` takes only the FQDN. `holdfast job set` fills a new job's schedule and retention from the default job (daily at 02:00 with 20 minutes' spread; one per day for 14 days, week for 8 weeks, month for 12 months, year for 3 years; the newest 3), and keeps an existing job's values for the flags not given. The API's `POST /hosts` drops its job; the UI's Add host asks for the FQDN, then opens the job form with the defaults. | learns two ways to make a job, or types the retention on every job | None. A new job runs at once whenever it arrives; a host without a job raises `HoldfastFirstBackupMissing`. |
| 5 | **No plaintext escrow bundle.** The maintenance key may read `escrow/`. `restore-export` brings back by itself every host in `escrow/` that the export lacks, as `host import` does. `holdfast escrow export` and `HoldfastEscrowOutdated` go, and with them spec §3.3's one root-socket exception. | keeps a plaintext file of every password; asks the storage admin to fetch escrow files during a rebuild; imports hosts added after the export one by one | The maintenance key can read ciphertext it cannot decrypt. |
| 6 | **The web UI works without an identity provider.** `holdfast ui-login`, as root on the server, prints a one-time link, valid 5 minutes, that opens an admin session; audited as `local:root` with `SUDO_USER`. The installer turns the UI on at `https://<server>/` with a certificate from Holdfast's own CA unless you give one. OIDC stays the way everyone else logs in, with groups and roles. | registers Holdfast at an OIDC provider and finds a browser-trusted certificate before seeing the UI | A browser warns about Holdfast's own CA until you trust it or give a certificate. The link is a bearer secret shown in a terminal, worth no more than root on the server, which it needs. |
| 7 | **Three alert-rule bugs**, fixed whatever else is decided. `HoldfastServerDown` says "Agents keep backing up", wrong since decision 041. `HoldfastStorageScrapeFailed` (`absent(…)`) and `HoldfastEscrowOutdated` fire for ever when the optional witness or bundle is not used. | gets pages that never stop | None. |

## 2. Not in M10, and why

- **LDAP in Holdfast.** Authelia, Dex and Keycloak put an LDAP or Active Directory behind OIDC in a few lines. Doing it in
  Holdfast adds bind secrets, group searches, trust for the LDAP server's certificate, and no second factor. The docs
  will show the provider route.
- **Local password users, as break glass.** A password store, brute-force throttling, a second factor of our own, and a
  shared login that survives offboarding. Break glass is root on the server: the CLI on its socket today, and the
  browser through `holdfast ui-login` (item 6). Below that, plain restic with the recovery file works with Holdfast gone.
- **One S3 key instead of two.** The gateway is the code every host talks to, hacked ones included; the key under it must
  not be able to delete.
- **SQLite instead of PostgreSQL.** A large change for little; the export already copes with PostgreSQL versions.
- **Off-site copies as a field of the job instead of a second instance with its own pages, history and `--offsite`.**
  It would remove a lot, but only for off-site users, and it is large. Later, if at all, as its own plan.

## 3. What an operator does, before and after

| | Now | After M10 |
| --- | --- | --- |
| Install | the bucket and 3 keys; `age-keygen`; the settings file; the server's line; the witness's line; Prometheus, rules, scrape config, CA copy; the mail script and 2 units, or Alertmanager; a dead man's switch; an OIDC client and a browser certificate for the UI; `/etc/holdfast` offline | the bucket and 2 keys; the settings file, with an SMTP relay or webhook and a heartbeat URL; the server's line; the recovery file offline; `holdfast ui-login` |
| A host | `host add` with its first job's flags | `host add`, then `job set` with the paths |
| A lost server | `/etc/holdfast` back with its modes; the line; `restore-export`; start; each host added after the export imported by hand from its escrow file | the line with the recovery file and the keys; `restore-export`; start |

## 4. Upgrading from M9

- `holdfast-freshness` leaves the release; remove the package where it runs, and its scrape job.
- The maintenance key gains `s3:GetObject` on `escrow/*`; B012 tests it.
- Scripts that use `host add --job …` call `job set` after it. `holdfast escrow export` is gone.
- A server installed with your own age key keeps it. `holdfast-server recovery-file` writes the recovery file of a running
  server (its credentials key; the age private key is yours already) for the new rebuild path.

## 5. Tests

- e2e: the server's witness metrics and the storage alerts without a witness; notifications to a test SMTP sink and a
  test webhook, once on start and once on end, and the heartbeat; job defaults and keeping values on a re-save; `host
  add` without a job; `restore-export` bringing back hosts from `escrow/`; `ui-login`'s link, once, within 5 minutes,
  audited.
- `make install-test`: an install without a recipient makes the recovery file and raises `recovery_file` until it is
  removed; the rebuild with the recovery file and the settings file; the UI answering with Holdfast's own certificate.
- `make rules-test` for the changed rules; the lab's tests as they are, with the rebuild walked with the recovery file.
- The documentation updated to match, and read again by someone who knows nothing of Holdfast.

## 6. Questions

Each has a proposal; "Go" accepts them all.

- **Q1. The witness.** *Proposal:* item 1, the server is the witness, and `holdfast-freshness` goes.
- **Q2. Alerts.** *Proposal:* SMTP and a webhook, either or both, plus the heartbeat URL; Prometheus stays optional.
- **Q3. The age key.** *Proposal:* the installer makes it unless you give a recipient (item 3).
- **Q4. The UI.** *Proposal:* on by default with Holdfast's own certificate, `ui-login` for root (item 6); no LDAP and no
  local users (section 2).
- **Q5. Order.** *Proposal:* 7, 4, 1, 5, 3, 6, 2: the bug fixes first, then the smallest changes, the alerts last.
