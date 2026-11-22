# 035: No custom roles

Status: accepted, 2026-11-22 (plan-m4 Q8; you answered "Go"). It decides OQ4.

## Decision

There are three roles, viewer, editor and admin, with the permissions of spec §6.2. Admins cannot define
roles of their own. Who may do what where is set with bindings: a group, one of the three roles, and host
patterns.

## Why

- **Three flat roles with per-host bindings already say who may do what where.** A custom role would add
  little beyond that.
- **Each extra role is one more thing to review,** and one more combination the permission tests must cover.

## Consequences

- OQ4 can be reopened if a real need comes up. A custom role would then be a named set of the existing
  permission names, so nothing in the permission checks would change.
