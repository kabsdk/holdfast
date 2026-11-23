# 024: Login through OIDC, with no sudo checks on hosts

Status: accepted, 2026-11-21. **Amended by decision 049 (2026-11-23):** OIDC is optional; without it, root logs in to
the UI with `holdfast ui-login`.

## Decision

- Login is through OIDC (Authelia), with groups taken from the groups claim. This works with or without
  LDAP.
- The second factor is the OIDC provider's job.
- Holdfast does not check sudo rights on the hosts. Permissions come only from Holdfast's role bindings.

## Why

- **The provider already does MFA.** Reimplementing it in Holdfast would add a weaker copy.
- **Sudo checks would make access depend on per-host state.** A user's rights would then depend on each
  host's sudoers files, which Holdfast neither controls nor audits. Permissions would be hard to predict, and
  checking them would require the host to be online.

*The reasons were inferred when the record was written. You accepted them on 2026-11-21, leaving the call to me ("Whatever you recommend").*

## Consequences

- Access is reviewed in one place: the role bindings, which are part of the config and audited.
