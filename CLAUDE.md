# CLAUDE.md: standing rules for Holdfast

Holdfast is a free, open-source control plane over restic for Linux hosts.

**Scope: a restic manager with sane defaults and good features, not a fix for everything that can go
wrong with restic** (decision 030). Do not add protections that restic itself does not offer. Where restic
can decide something, such as what retention keeps, let it decide; do not re-implement it.

**Who it is for, and how it runs** (decision 042): a small ops team with roughly ten to a few hundred Linux servers
and S3 storage of its own, who want one place to set up, see and restore restic backups. **One server, no high
availability**: no clustering, standby, second gateway or Kubernetes deployment. While the server is down, backups
wait; a lost server is replaced by a new one on the same bucket. Do not propose high-availability work.

| What | Where |
| --- | --- |
| Behaviour | [docs/spec.md](docs/spec.md) |
| Decisions and their reasons | [docs/decisions/](docs/decisions/) |
| The documentation for people who run it | [website/index.html](website/index.html), published at https://kabsdk.github.io/holdfast/ |
| Building, testing, releasing | [CONTRIBUTING.md](CONTRIBUTING.md) |
| What each release changed | [CHANGELOG.md](CHANGELOG.md) |

Read the spec, and the decisions it points to, before changing anything.

## How to work

1. **Plan before code.** For anything larger than a fix, write a plan first: what changes, and everything that is
   contradictory or underspecified, as questions. Wait for an answer before writing product code. A plan is a
   working paper: it lives in the issue or the pull request, not in the repository. When the work ships, its answers
   are in `docs/spec.md`, and the ones that are decisions in `docs/decisions/` (decision 051).
2. **Never decide an open question yourself.** That covers the unanswered questions in a plan. If something seems
   wrong, say so and argue it. Do not silently work around it.
3. **Test end to end.** Every feature ships with end-to-end tests in `test/e2e` that run against real restic (pinned
   version), a real S3 server (MinIO) and PostgreSQL. They already cover metadata and ACL fidelity, server outage
   with spooling, SIGKILL mid-upload, retention and the storage witness. The installer and the Ansible collection
   have their own tests (`make install-test`, `make ansible-test`).
4. **One change per pull request.** When behaviour changes, update `docs/spec.md` and the documentation in
   `website/index.html` in the same pull request, and add a line to `CHANGELOG.md`. A new decision gets a new
   `docs/decisions/NNN-*.md` record that gives its reason.

## Rules the code must never break

- **UI = API = CLI.** Every option and every action exists in all three, with the same names, the same
  checks and the same check codes. Any exception must be listed in spec §3.3.
- **API first.** The UI and the CLI are thin clients of the HTTP API. Build every feature as an API endpoint
  first, and keep logic, validation and checks behind the API, never in a client. See decision 028.
- **No file contents leave a host through Holdfast.** Only names and metadata do, and Changes never shows
  contents. `content_view` was cut (decision 030). Restores run on an agent.
- **Every action is audited.** Each one is written to the host's history and to the audit log, with the
  actor, the time, and any accepted check codes.
- **Nothing destructive bypasses the 7-day protection, except an admin's Prune now, Delete now or repository
  rollback** (decision 038: a rollback deletes no S3 version, and can itself be rolled back). That covers
  forget, prune, retention cuts, deleting a snapshot (decision 031) and removal, however they are triggered.
  Removing a job or a host never deletes data; only decommission does. Off-site copies, which are optional, are
  forgotten only by their own retention, behind the 7-day protection, or by an admin's Prune now or Delete now
  there; deleting a snapshot in primary never deletes its copy (decision 041).
- **Hosts hold no S3 key.** They back up through the server's gateway with their own pinned certificate, and the
  gateway decides what a host may do: read and add to its own repository, delete only its own lock files, never
  rewrite a file or write config or keys (decision 041). Off-site stays optional: without `offsite:` nothing about
  it is shown, checked or alerted.

These also follow from the spec:

- Credentials never appear in the UI, in API or CLI output, in logs, in metrics or in config revisions.
- Agents never run forget or prune. Only the maintenance leader does.
- Holdfast runs no command on a host: no dumps, no pre-backup hooks (decision 052). Preparing data, such as a
  database export, is the host's job, with its own timer and monitoring; Holdfast backs up the files.
- There is no raw restic console and there are no free-form restic arguments.
- Passwords are never passed in argv, and restic never inherits `RESTIC_*` or `AWS_*` variables from its
  parent process.
- `restic unlock` only ever removes stale locks. Never use `--remove-all`.
- Only show what the job's engine declares it can do.

## Repository

- The product is at the repository root: `cmd/`, `internal/`, `test/`, `packaging/`, `deploy/`, `scripts/`, the
  Ansible collection in `ansible/`, the documentation site in `website/` and the VM lab in `lab/`.
- Releases are made with `scripts/release.sh <version>`; see CONTRIBUTING.md. The website is published to GitHub
  Pages from `main` by `.github/workflows/pages.yml`.
- Holdfast is under Apache-2.0 (decision 043). Anything linked into a program or vendored into the UI must be under a
  licence that allows that: MIT, BSD, ISC, Apache-2.0, MPL-2.0. The GPL family, the SSPL and the BUSL are not allowed;
  `scripts/third-party-notices.sh` stops the package build if one appears.
- Holdfast was built in ten milestones, M1 to M10, up to 0.5.0, the first release. Decision records and comments
  still name them; the plans themselves are in the git history (the tag v0.5.0 still has them).
