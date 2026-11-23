# 036: Repository passwords escrowed in the bucket, encrypted with age

Status: accepted, 2026-11-22. It decides the escrow bundle's format, which M1 left open, and builds decision
030's "at host add, the repository password, encrypted to an offline `age` recipient, written to the bucket".
**Amended by decision 047 (2026-11-23):** the installer makes the age key unless you give one, and keeps it in the
recovery file; the plaintext bundle (`holdfast escrow export`) is gone; the maintenance key may read `escrow/`.

## Decision

- `server.yaml` names one `age` recipient: a public key. Its private key is kept offline by the admin.
- At host add, and whenever a repository password changes, the server writes `escrow/<fqdn>.age` to the
  bucket: the FQDN, the repository URL and the repository password, encrypted to that recipient.
- The S3 key is not in it. The admin makes the S3 keys, and can make a new one.
- Without a recipient nothing is written, and the host's page and a metric warn.
- The manual escrow export on the root socket stays.

## Why

- **A lost PostgreSQL must not lose a repository.** With escrow only by manual export, a host added after
  the last export could not be read again.
- **The bucket is already the safe place.** It is versioned, and no Holdfast key can delete a version for
  good (decision 029), so the file survives a compromised server.
- **age is small, audited, and needs only a public key on the server.** The server can encrypt but never
  decrypt, so a compromised server cannot read old escrow files.

## Consequences

- The maintenance key needs write access to `escrow/`. The storage contract grows a check, and
  `docs/storage.md` its policy.
- Rebuilding Holdfast (M7) and the bare-metal runbook (M6) read these files with the offline key.
