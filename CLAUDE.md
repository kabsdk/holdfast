# CLAUDE.md: standing rules for Holdfast

Holdfast is a free, open-source control plane over restic for Linux hosts.

**Scope: a restic manager with sane defaults and good features, not a fix for everything that can go
wrong with restic** (decision 030). Do not add protections that restic itself does not offer. Where restic
can decide something, such as what retention keeps, let it decide; do not re-implement it.

| What | Where |
| --- | --- |
| Behaviour | [docs/spec.md](docs/spec.md) |
| Decisions and their reasons | [docs/decisions/](docs/decisions/) |
| Not decided | [docs/open-questions.md](docs/open-questions.md) |
| The current milestone's plan | `docs/plan-mN.md` |

Read the spec and the current plan before changing anything.

## How to work

1. **Plan before code.** For each milestone, write `docs/plan-mN.md`. List everything that is contradictory
   or underspecified as questions, then wait for an answer before writing product code.
2. **Never decide an open question yourself.** That covers the questions in `docs/open-questions.md` and the
   unanswered ones in a plan. If something seems wrong, say so and argue it. Do not silently work around it.
3. **Test end to end.** Every feature ships with end-to-end tests that run against real restic (pinned
   version) and a real S3 server (MinIO in CI). Reuse and extend the PoC's tests in `poc/test/e2e`. They
   already cover metadata and ACL fidelity, server outage with spooling, SIGKILL mid-upload, the retention
   model and the storage witness.
4. **One milestone per PR.** When behaviour changes, update `docs/spec.md` in the same PR. A new decision gets
   a new `docs/decisions/NNN-*.md` record that gives its reason.

## Rules the code must never break

- **UI = API = CLI.** Every option and every action exists in all three, with the same names, the same
  checks and the same check codes. Any exception must be listed in spec §3.3.
- **API first.** The UI and the CLI are thin clients of the HTTP API. Build every feature as an API endpoint
  first, and keep logic, validation and checks behind the API, never in a client. See decision 028.
- **No file contents leave a host through Holdfast.** Only names and metadata do, and Changes never shows
  contents. `content_view` was cut (decision 030). Restores run on an agent.
- **Every action is audited.** Each one is written to the host's history and to the audit log, with the
  actor, the time, and any accepted check codes.
- **Nothing destructive bypasses the 7-day protection, except an admin's Prune now.** That covers forget,
  prune, retention cuts, library updates and removal, however they are triggered. Removing a job or a host
  never deletes data; only decommission does.

These also follow from the spec:

- Credentials never appear in the UI, in API or CLI output, in logs, in metrics or in config revisions.
- Agents never run forget or prune. Only the maintenance leader does.
- There is no raw restic console and there are no free-form restic arguments.
- Passwords are never passed in argv, and restic never inherits `RESTIC_*` or `AWS_*` variables from its
  parent process.
- `restic unlock` only ever removes stale locks. Never use `--remove-all`.
- Only show what the job's engine declares it can do.

## Milestones

| Milestone | Scope | Done when |
| --- | --- | --- |
| M1 | Agent and server core: enrollment, config revisions in PostgreSQL, local schedule, backup, event spool, storage witness, Prometheus. | The PoC's e2e tests pass on the real codebase. |
| M2 | Snapshot catalogue (as a cache), snapshot list, Files, Changes, restore via the agent, and a CLI command for each. | The restore and metadata/ACL tests pass on Ubuntu, RHEL and Oracle Linux test VMs. |
| M3 | Checks engine, protection items, retention, the 7-day protection, Prune now. | |
| M4 | API with OIDC and roles, then the UI built from `mockup/App.dc.html`. Every UI action has a matching API call and CLI command. | |
| M5 | The library, profiles and "Jobs with the old values" were cut (decision 030). To be re-planned with what 030 adds: dumps, restore to another host, rollback from S3 versions, run controls. | |
| M6 | Off-site copies and rebuilding Holdfast. No standby failover (decision 030). Starts only after the open questions are decided. | |

## Repository

- `poc/` is the proof of concept: Go standard library only, tested against restic 0.18.1. It is the
  reference for porting, so leave it as it is. The product code lives at the repository root (`cmd/`,
  `internal/`, `test/e2e/`).
- `mockup/App.dc.html` is the UI mockup with mock data. Use it as a reference for behaviour, not for pixels.
- Run the PoC's e2e tests from `poc/`:
  `sudo -E HOLDFAST_RESTIC=/path/to/restic go test ./test/e2e/ -count=1 -v -timeout 10m`
