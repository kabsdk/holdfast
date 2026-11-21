# 025: Three roles with flat permission sets

Status: accepted, 2026-11-21. Refined by [decision 033](033-what-each-role-sees.md): `view` is split into
`view` (existence and status) and `browse` (file names, for editors and admins), and a host without a
binding is invisible.

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

*The reasons were inferred when the record was written. You accepted them on 2026-11-21, leaving the call to me ("Whatever you recommend").*

## Consequences

- Whether admins can define custom roles is open question OQ4.
