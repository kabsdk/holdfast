# 014: UI = API = CLI, and every action is audited

Status: accepted, 2026-11-21

## Decision

- **Parity.** The options in the UI, the API and the CLI are the same, with very few exceptions. Each
  exception is listed in spec §3.3.
- **Same checks everywhere.** All three use the same checks engine and the same codes. The CLI takes
  accepted warnings explicitly (`--accept R102,R104`), and without them it refuses and prints the same
  explanation as the UI.
- **Audit.** Every action is recorded in the host's history and in the audit log.

## Why

- **Automation and people get the same power and the same guard rails.** No option is reachable only
  through a side door that skips the checks.
- **Every change can be traced** to a person, a time and the warnings they accepted.

*The reasons are inferred. Confirm or correct them.*

## Consequences

- A feature is not done until it exists in all three interfaces (from M4 onwards, once the API and UI
  exist).
- Server-root-only operations, such as editing `server.yaml`, are exceptions and must be listed in the spec.
