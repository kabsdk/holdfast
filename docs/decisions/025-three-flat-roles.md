# 025: Three roles with flat permission sets

Status: accepted, 2026-11-21

## Decision

There are three roles: viewer, editor and admin. Each has an explicit, flat set of permissions, and there
is no inheritance between them (spec §6.2).
- A binding grants a role on a list of host patterns.
- Several bindings combine per host.
- The default bindings map `holdfast-admins`, `holdfast-editors` and `holdfast-viewers` to their roles on
  `["*"]`.

## Why

- **What a role can do is in one list.** There is no need to follow an inheritance chain.
- **Per-host bindings fit how teams own hosts.**

*The reasons are inferred. Confirm or correct them.*

## Consequences

- Whether admins can define custom roles is open question OQ4.
