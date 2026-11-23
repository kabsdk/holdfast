# Security policy

## Reporting a problem

Please report a security problem privately, never in a public issue: use **Report a vulnerability** on the
repository's Security tab, [github.com/kabsdk/holdfast/security/advisories/new](https://github.com/kabsdk/holdfast/security/advisories/new).
Say which version you run, what an attacker could do, and how to reproduce it. Do not include real secrets.

The fix is released as a new version, and the advisory is published with it, crediting you if you want.

## Supported versions

Fixes go into the next release. Only the latest release is supported; upgrade to it first
([Upgrading](https://kabsdk.github.io/holdfast/#upgrade)).

## What counts

Anything that breaks the security model in the documentation ([Security](https://kabsdk.github.io/holdfast/#security)),
for example:

- a host that can read, change or delete another host's repository, or delete anything of its own but lock files;
- credentials in the web UI, in API or CLI output, in logs, in metrics or in config revisions;
- file contents that leave a host through Holdfast;
- a deletion that skips the 7-day protection without an admin's Prune now, Delete now or rollback;
- an action that is not in the audit log;
- a way into the web UI or the API without a valid login.

Weaknesses of restic itself belong to [restic](https://github.com/restic/restic/security), and those of your S3
storage to its vendor.
