# 028: API first; the UI and the CLI are thin clients of the API

Status: accepted, 2026-11-21

## Decision

- **Every option and every action is an HTTP API endpoint first.** The UI and the CLI only call the API.
- **The UI and the CLI hold no logic of their own** beyond presentation: no business rules, no validation, no
  checks.
- **The server runs the checks.** It returns the check results and their codes through the API, and the UI
  and CLI render them.
- **Holdfast Vault follows the same rule** for its own API, UI and CLI.

## Why

- **"UI = API = CLI" holds by construction.** When both clients are thin, a feature cannot exist in one and be
  missing from another, and the two clients cannot disagree.
- **There is one place to look.** Validation, checks, permissions and audit all live behind the API, so there
  is one place to review and test them.

## Consequences

- A feature is built in this order: API endpoint, then CLI command, then UI.
- The end-to-end tests drive the product through the API, or through the CLI, which is a client of the API.
- Agents use their own, separate protocol (`/agent/v1`). It is not part of the user-facing API.
