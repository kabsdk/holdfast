# 021: One checks engine, with stable codes

Status: accepted, 2026-11-21

## Decision

**One engine for every interface.** Config checks run in one engine, shared by the UI, the API and the
CLI.

**Every check has a stable code.**

| Severity | Codes | Effect |
| --- | --- | --- |
| Error | R001 to R005, S001, S003 | Blocks saving. |
| Warning | S002, R006, R101 to R104 | Must be accepted one by one. |
| Info | I001 | Shown only. |

**Accepted codes are recorded** in the revision and in the audit log.

## Why

- **Stable codes make warnings scriptable and auditable.** A script can accept exactly the warnings its
  author reviewed (`--accept R102,R104`), and the audit log shows which warnings someone chose to override.
- **One engine means the interfaces cannot disagree** about what is allowed.

*The reasons were inferred when the record was written. You accepted them on 2026-11-21, leaving the call to me ("Whatever you recommend").*

## Consequences

- A code is never reused or renumbered. A retired check keeps its code unused.
