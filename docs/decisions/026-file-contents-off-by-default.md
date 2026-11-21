# 026: No file contents by default

Status: superseded by [decision 030](030-simplify-after-the-architecture-review.md), 2026-11-21:
`content_view` is cut, and no file contents ever leave a host through Holdfast.

## Decision

**By default, no file contents leave a host.**
- Files shows names and metadata only (`restic ls --long`).
- Changes shows counts only, never contents.
- No content appears in the UI, the API or the CLI.

**Contents can be shown only when `content_view.enabled` is true** in `/etc/holdfast/server.yaml`, which
needs root on the server to change. Even then, three limits apply:
- the viewer has one of `content_view.roles`;
- the file's mode has all of `content_view.require_mode_bits` (the default is world-readable, `0004`);
- the file is no larger than `content_view.max_file_size`.

**A built-in deny always applies** and cannot be configured: `/etc/holdfast/**` and the repository password
files.

## Why

- **Backups hold everything on a host,** including secrets, keys and personal data. A backup console that
  shows contents would be a way to read any file on any host.
- **Switching it on is a server-root decision.** Keeping it off by default and in `server.yaml` means it
  cannot be enabled from inside the product.
- **The default mode bits limit what can be shown.** Requiring world-readable means only files that every
  user on the host could already read.

*The reasons are inferred. Confirm or correct them.*

## Consequences

- Restore is how you see contents: on the host, by root.
