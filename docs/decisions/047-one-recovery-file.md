# 047: One recovery file, and no escrow bundle

Status: accepted, 2026-11-23 (plan-m10 items 3 and 5). Amends decisions 036, 040 and 044. You asked: "why does the
user need to generate keypairs? Why not just a join token like k3s or something? Is it because of security? Restore?"

## Decision

1. **The installer makes the age key** unless `HOLDFAST_ESCROW_RECIPIENT` names one. It writes
   `/root/holdfast-recovery.txt` (mode 0600): the age private key and the server's credentials key. `server.yaml`'s
   `recovery_file` names it, and the server shows the problem `recovery_file` until the file is gone from the server:
   it belongs offline, such as in a password manager. With your own recipient, `holdfast-server recovery-file -config`
   writes the credentials key to the file and you add your age key's line to it.
2. **A rebuild is the install line** with the settings of the first install and `HOLDFAST_RECOVERY_FILE`. The nightly
   export now carries the server's TLS files (the agents' CA among them) and `server.yaml` without the database's
   address; `restore-export` writes the files that are missing, never overwriting one, writes the credentials key
   from the recovery file, and loads the database. The agents reconnect by themselves. Putting a copy of
   `/etc/holdfast` back still works.
3. **Hosts added after the export come back by themselves.** The maintenance key may read `escrow/`; `restore-export`
   brings back every host whose escrow file is there but which the export lacks, as `host import` does. They enroll
   again with a new code.
4. **No plaintext escrow bundle.** `holdfast escrow export`, its root-socket exception in spec §3.3 and
   `HoldfastEscrowOutdated` go.

The credentials key never goes into the bucket. age encrypts but does not authenticate: anyone with the maintenance
key could write an export that the age key opens. The credentials key opening the export's sealed passwords is what
proves the export was the server's.

## What it costs

The age key is born on the server, not offline. Whoever controls the server at install time has every repository
password already, so this gives them nothing new; the problem `recovery_file` stands in for "offline from minute
zero". The maintenance key can read escrow ciphertext it cannot decrypt.

## Why

- **The key pair is for restore, not for joining.** Agents join with a one-time code (decision 041). The age key is
  what opens the escrowed repository passwords and the nightly export when the server is gone; it must exist outside
  the server. Making it for the admin keeps that and drops a tool to install and a step to get wrong.
- **One thing to keep, not three.** Before: the age key, an offline copy of `/etc/holdfast` with plaintext S3 keys and
  the database's password, and a plaintext bundle of every password. Now: one file, plus the settings you installed
  with.

## What changes

`holdfast-server recovery-file`; `server.yaml`'s `recovery_file`, `tls.ca_file` and `tls.ca_key_file`; the export's
files; `restore-export`'s recovery file and hosts from `escrow/`; storage policy and B012 (`s3:GetObject` on
`escrow/*`); the installer's `HOLDFAST_RECOVERY_FILE`; the runbooks.
