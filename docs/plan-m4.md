# Plan for M4: login, roles, and the UI

Status: draft, 2026-11-22. Waiting for your answers to the questions in §7. No product code is written
until they are answered.

## 1. Goal and done when

**Goal.** People use Holdfast from a browser and from the CLI on their own machine, each with exactly the
rights their role gives them on each host:
- login is through OIDC (decision 024);
- the three roles and their permissions are enforced behind the API (spec §6.2, decisions 025 and 033);
- the UI is built from `mockup/App.dc.html`, and every UI action is an API call that also has a CLI command
  (decisions 014 and 028).

**Done when:**
- every API endpoint has a permission, and a test proves that each role can do exactly what spec §6.2 says,
  on the hosts its bindings cover, and that a host without a binding does not exist for you;
- the CLI logs in over HTTPS with the provider's device flow, and runs every command with the user's own
  rights;
- the UI covers every page of the mockup that is still in scope (§5), and a browser test drives each page
  against a real server, a real restic and a real S3;
- a parity test lists every API endpoint with its CLI command and its UI element, and fails if one is
  missing.

## 2. Scope

| Feature | API | CLI |
| --- | --- | --- |
| Browser login and logout (OIDC authorization code flow with PKCE) | `GET /auth/login`, `GET /auth/callback`, `POST /auth/logout` | |
| CLI login (OIDC device flow, through the server) | `POST /auth/device`, `POST /auth/device/token` | `holdfast login --server <url>`, `holdfast logout` |
| Who am I, and what may I do where | `GET /api/v1/me` | `holdfast whoami` |
| Role bindings (`config.access`) | `GET /api/v1/access`, `PUT /api/v1/access` | `holdfast access list`, `holdfast access set` |
| Permissions on every existing endpoint (§4) | all of `/api/v1` | all |
| A host without a binding is invisible (decision 033) | lists, search, counts, audit, revisions | the same |
| The UI | served by `holdfast-server` | |
| Remove a job, remove a host (no data is deleted, spec §3.5) — see Q7 | `DELETE …/jobs/{job}`, `DELETE /api/v1/hosts/{fqdn}` | `holdfast job remove`, `holdfast host remove` |

**Not in M4:** decommission (Q7), custom roles (OQ4, Q8), automation tokens (Q4), anything decision 030 cut.

## 3. Design in brief

**One relying party: the server.** The server is the only OIDC client. It does the code flow for browsers
and the device flow for the CLI, checks the ID token, reads the groups claim, and gives the browser a
session cookie or the CLI a session token. The API accepts either. Nothing but the server talks to the
provider, so any OIDC provider with these two flows works: Authelia (4.39 or later), Keycloak, Dex, Okta.

**Sessions** are rows in PostgreSQL: who (the `sub` and the username), their groups at login, when the
session started and ends, and its kind (browser or CLI). The token is stored hashed. Logout deletes the
row. Groups are read only at login, so a group change takes effect at the next login (Q3).

**The root socket stays.** On the server, root's `holdfast` keeps using the Unix socket. Its peer is admin
on every host, as today: root can read the database and the sealing key anyway, so treating it as less
would be pretend security. Spec §3.3's list of exceptions to UI = API = CLI gets its first entries, all of
them root on the server: escrow export, which stays socket-only, and editing `server.yaml`. Login itself
differs by client (a browser redirect, a device code), but both end in the same session.

**Permissions are checked in one place**, a table from endpoint to permission (§4), before the handler
runs. Host-scoped endpoints check the binding for that host. A host you cannot see answers 404, the same
as a host that does not exist, so its name cannot be probed.

**What you may do is part of every answer.** Each host in `GET /api/v1/hosts` and each job carries the
actions you may take on it, such as `["view","browse","backup.run"]`. The UI shows a button only if the
action is there. It does not work out permissions itself (decision 028).

**Audit.** The actor becomes the OIDC username, with the `sub` in the entry's detail, and the source
`ui`, `cli` or `api`. Login, logout and refused requests for a permission you lack are audited too.

**The UI** is a single-page app served by `holdfast-server` from files embedded in the binary. It only calls
`/api/v1` (§5, Q5). The browser and the API share one origin, so cookies are `HttpOnly`, `Secure` and
`SameSite=Strict`, and every request that changes something must carry a matching `Origin` header.

## 4. Permissions

Following spec §6.2. The endpoints from M1–M3 get their check; the ones M4 adds are at the end.

| Endpoint | Permission |
| --- | --- |
| `GET /status`, `GET /me` | logged in |
| `GET /hosts`, `GET /hosts/{fqdn}`, `…/events`, `…/snapshots`, `…/pending`, `…/prunes`, `GET /prunes`, `GET /search` | `view` (lists filtered to your hosts) |
| `GET …/snapshots/{id}/files`, `…/changes`, `GET …/find` | `browse` |
| `GET /restores` | `view`; the paths left out or restored only are shown with `browse` (they are file names) |
| `POST …/jobs/{job}/run` | `backup.run` |
| `POST …/snapshots/{id}/restore` into a new directory | `restore.new_dir` |
| `POST …/snapshots/{id}/restore` in place, or into another directory | `restore.in_place` (Q6) |
| `PUT …/jobs/{job}`, `POST …/jobs/{job}/check`, `POST …/jobs/{job}/keep-longer` | `config.jobs`; a save that accepts R104 also needs `retention.cut` |
| `GET` and `POST …/prune-now`, `POST …/retention/run` | `retention.cut` |
| `POST` and `DELETE …/snapshots/{id}/delete` | `snapshot.delete` |
| `POST /hosts` (add host), `PUT …/s3-key`, `POST …/enrollment-code` | `config.storage` (Q6) |
| `POST …/acknowledge` | admin on that host (Q6) |
| `GET /revisions`, `…/{n}`, `…/diff/{b}` | `view`, cut down to your hosts (Q2) |
| `GET /audit` | `view`, cut down to your hosts; entries with no host are for admins on `*` |
| `POST /maintenance/check` | admin on every host it touches (Q6) |
| `POST /escrow/export` | root socket only |
| *New in M4:* `GET /settings` | logged in |
| `GET` and `PUT /access`, the session list, ending a user's sessions | `config.access` |
| `POST …/snapshots/{id}/restore-pair` (Q11) | `restore.new_dir` |
| `DELETE …/jobs/{job}`, `DELETE /hosts/{fqdn}` (Q7) | admin on that host |

## 5. The UI

The mockup is a reference for behaviour. Each page below lists what it shows, and each action with its API
call and CLI command. An action you may not take is not shown (§3). Pages poll the API every 10 seconds
while open; there is no live push.

### 5.1 Pages from the mockup

| Page | Shows | Actions → API → CLI |
| --- | --- | --- |
| **Top bar** | search; you, your role, your provider | search → `GET /search` → `holdfast search`; log out → `POST /auth/logout` → `holdfast logout` |
| **Fleet** | counts of jobs OK, stale, failed, incomplete; hosts that need attention, or all; per host its jobs' status, last success, agent seen | Add host (below); Check all repositories now → `POST /maintenance/check` → `holdfast check` |
| **Host** | agent, OS, restic, revision applied; banners (storage, drift, disappeared snapshots, foreign keys, not made by Holdfast, restic not pinned); job cards with paths, schedule, retention, last success, newest snapshot in the repository, next run, a pending cut; recent runs, with who started each | Back up now → `POST …/jobs/{job}/run` → `holdfast job run`; Snapshots; Restore from latest; Edit; Add job; Keep longer → `POST …/keep-longer` → `holdfast job keep-longer`; Prune now (below); Acknowledge → `POST …/acknowledge` → `holdfast host acknowledge`; Replace S3 key → `PUT …/s3-key` → `holdfast host set-key`; New enrollment code → `POST …/enrollment-code` → `holdfast host enroll-code`; Run retention now → `POST …/retention/run` → `holdfast retention run`; Remove job, Remove host (Q7) |
| **Job snapshots** | snapshots by month, 25 a page; for each, the rule that keeps it and a pending prune (Q11); find a file in all snapshots | open a snapshot; find → `GET …/find` → `holdfast snapshot find` |
| **Snapshot** | details; what happens to it (kept, pending, deleted, pending deletion); tabs Files, Changes, Restore | Files → `GET …/files` → `holdfast snapshot files`; Changes → `GET …/changes` → `holdfast snapshot changes`; Restore, dry run → `POST …/restore` → `holdfast restore`; Restore only this; Restore both versions to diff on the host (Q11); Delete, Cancel deletion, Delete now → `POST`/`DELETE …/delete` → `holdfast snapshot delete` |
| **Restores** | every restore you can see; a host → job picker | the picker leads to the job's snapshots → `GET /restores` → `holdfast restores` |
| **Configure** (one tab per job, and Add job) | paths, excludes, schedule, retention; the checks as you edit; what you get (worst-case loss, restore points, oldest point, stale alert, the restic flags); the diff of your change; the CLI command for the same change | check → `POST …/check` → `holdfast job check`; accept a warning → `accept`; save → `PUT …/jobs/{job}` → `holdfast job set` |
| **Config history** | retention cuts waiting out the 7 days (Q11); revisions; diff of any two; past prunes | Keep longer; Prune now; diff → `GET /revisions/{a}/diff/{b}` → `holdfast revision diff`; past prunes → `GET /prunes` → `holdfast prunes` |
| **Prune now** | the count deleted and kept, the date range, every snapshot by month, the restic commands | type the short name → `POST …/prune-now` → `holdfast prune-now --confirm` |
| **Add host** | FQDN; the host's S3 key (decision 029); an optional first job; then the enrollment code, the install command, and progress: waiting → connected → first backup done | add → `POST /hosts` → `holdfast host add`; the storage checks' findings if the key fails |
| **Problems** (the mockup's Alerts, Q11) | each problem the server knows of, per host | open the host |
| **Audit log** | when, who, action, host, reason; filter by host and action; pages | → `GET /audit` → `holdfast audit` |

### 5.2 Pages the mockup lacks

| Page | Why | Actions → API → CLI |
| --- | --- | --- |
| **Access** | bindings are config (spec §6.3), `config.access` | list, edit → `GET`/`PUT /access` → `holdfast access`; sessions, end a user's sessions (Q3) |
| **Settings**, read-only | what `server.yaml` says, without secrets: storage, the lifecycle rule's days, check and retention modes, the default zone, the provider and groups claim. Changing them needs root (spec §3.3) | → `GET /api/v1/settings` → `holdfast settings` |

### 5.3 What the UI leaves out

- **Cut by decision 030:** the Library page, "Add from library", the protection cards, profiles on Add host,
  "Jobs with the old values", the library audit actions, "Standby site B in sync", everything about the vault
  and site B. The Changes tab's mock line diffs stay out too: Changes is restic's file-level diff (spec §5.3).
- **Not Holdfast's to show:** "Storage witness scraping" and "witness disagrees". The witness is judged by
  Prometheus, apart from the server, on purpose (decisions 008 and 009). The UI shows the server's own view
  instead: the newest snapshot in the repository as the server last read it.
- **Different from the mockup:** the install command is `holdfast-agent enroll … --ca-sha256 …`, not
  `curl | sh`, because the agent never downloads anything (decision 032). Restic 0.19.1, not 0.18.1.

### 5.4 Additions to the API that the UI needs

So that the UI stays free of logic (decision 028), the API also gives:
- `POST …/jobs/{job}/check` returns, with the findings, what the job gets (next runs, longest gap, worst-case
  loss, stale alert, estimated restore points, oldest restore point, the restic flags), the config diff of the
  change, and the equivalent `holdfast job set` command;
- every host and job carries the actions you may take on it (§3), and its problems (Q11);
- recent runs carry who started each, from the command that asked for it;
- every action that changes something takes an optional `message`, the "Reason" in the audit log; restores
  get it too.

## 6. Tests

- **Permissions, e2e:** three users in a real OIDC provider (a pinned Authelia container in CI, with a file
  user database), in `holdfast-admins`, `holdfast-editors` and `holdfast-viewers`, plus one bound to a single
  host. For every endpoint, each user gets exactly what §4 says, including 404 for a host it cannot see.
- **CLI login, e2e:** `holdfast login` against the same provider, then a command with the user's rights.
- **UI, e2e:** a headless browser (Playwright in a container) logs in as each role and goes through each
  page; one action per page is carried out for real, against real restic and MinIO, as the other e2e tests do.
- **Parity:** a test reads the route table, the CLI's commands and the UI's list of calls, and fails if an
  endpoint is missing from either client, unless spec §3.3 lists it as an exception.
- **Unit:** the binding matcher (patterns, several bindings, no binding), session expiry, the `Origin` check.

## 7. Questions

**Q1. Where people connect.** Agents use the agent listener (`listen`, port 8443), with a certificate they
pin. Browsers need a certificate from a CA they already trust, and the UI should be reachable only from
admin networks, while every host must reach the agent port.
- *Proposal:* a second listener for the UI and the API, `ui.listen` (such as `:443`), with its own
  certificate and key. It is off until configured; the root socket works either way.
- *The alternative:* one port for agents and people, with one certificate.

**Q2. Revisions and the audit log, for someone who cannot see every host.** A revision holds the whole
config, so showing it as it is would name every host (decision 033).
- *Proposal:* the revision list shows only revisions that changed something you can see. A revision's
  config and its diffs show only your hosts, and the access section only with `config.access`. Audit entries
  are cut down the same way. An admin on `*` sees everything.
- *The alternative:* everyone who can log in sees every revision in full.

**Q3. Sessions.**
- *Proposal:* a browser session lasts 12 hours and ends after 1 hour without use; a CLI session lasts 7
  days. Groups are read at login, so removing someone from a group takes effect at their next login, or at
  once if an admin ends their sessions: `holdfast access sessions` and `holdfast access end-sessions
  <user>` (`config.access`).
- *The alternative:* shorter sessions, and no list of sessions.

**Q4. Automation without a person,** such as Ansible adding hosts.
- *Proposal:* not in M4. Scripts on the server use the root socket, as today. Tokens for remote
  automation can come later, if you need them.
- *The alternative:* per-user API tokens now, created in the UI, with that user's rights.

**Q5. How the UI is built.**
- *Proposal:* no build step. Plain ES modules with Preact and htm, vendored at pinned versions (about 15
  KB), embedded in `holdfast-server` with `go:embed`. No Node.js is needed to build or run Holdfast.
- *The alternative:* React, TypeScript and Vite. Familiar to more people, but it needs Node.js and a build
  step in CI and in every release.

**Q6. Permissions the spec does not name.** Proposals:
- adding a host, replacing its S3 key and issuing an enrollment code are `config.storage` (admin): they
  handle the host's key, and an enrollment code hands the host's credentials to whoever redeems it;
- a restore into another directory is `restore.in_place` (admin): it writes outside `/var/restore`;
- clearing the "changed outside Holdfast" alerts is admin: it accepts a foreign repository key as known;
- `holdfast check` and `holdfast retention run` are admin, on each host they touch;
- checking a job and Keep longer are `config.jobs`, like saving one.

**Q7. Remove and decommission.** Spec §6.2 gives admins "remove host / job", and §3.5 says removing never
deletes data, while decommission does, after a grace period and a typed FQDN. None of these is built.
- *Proposal:* M4 builds remove job and remove host.
  - A removed job leaves the config. Its snapshots stay, and the daily run no longer touches them.
  - A removed host is retired: its agent's token is revoked, and its repository stays and can still be
    browsed. Adding the same FQDN again brings it back, with its repository, after a new enrollment.
  - Decommission, the only path that deletes a whole repository, gets its own plan. Its grace period is not
    decided.
- *The alternative:* decommission in M4 too, with a 7-day grace period like the other deletions.

**Q8. OQ4: custom roles.** This is an open question, so I'm asking.
- *Proposal:* no custom roles. Three flat roles with per-host bindings cover "who may do what where", and
  each extra role is one more thing to review. OQ4 can be reopened if a real need comes up.

**Q9. The OIDC provider in the tests and the lab.**
- *Proposal:* the e2e tests run a pinned Authelia container with a file user database, so login is tested
  against a real provider, as restic and S3 are. The lab gets Authelia on hfsrv.
- *The alternative:* a small fake provider written in Go. Faster, but it proves less.

**Q10. `auth.bootstrap_admins`** (spec §6.4).
- *Proposal:* usernames listed there are admin on every host, whatever their groups. It exists for the
  first login, before the provider has the groups. Changing it needs root on the server, and every login
  that uses it is audited.

**Q11. Smaller calls from the mockup.** Proposals:
- **Alerts becomes Problems.** Holdfast has no Alertmanager. The page lists the problems the server knows of,
  per host: stale, failed, incomplete, storage checks failing, drift, snapshots disappeared, foreign keys,
  restic not pinned, retention failing. The same list drives Fleet's "Needs attention". Paging stays with
  Prometheus.
- **"Kept by" in the snapshot list** comes from restic: `forget --dry-run --json` says why it keeps each
  snapshot (such as "daily snapshot"). The job's snapshot list carries those words, the pending date, and pages
  of 25.
- **The waiting cuts in Config history** are listed from the revisions, without asking restic: host, job, old →
  new retention, and the date from which they may go. Opening one shows the host's pending view, with the
  counts.
- **A new job starts from one default:** daily at 02:00 with a 20-minute spread; keep one per day for 14 days,
  per week for 8 weeks, per month for 12 months, per year for 3 years, and the newest 3. With Q12 as proposed,
  it passes every check with no warnings. The mockup's protection cards came from the library, which was cut.
- **Restore both versions to diff on the host** becomes one endpoint, `POST …/snapshots/{id}/restore-pair`
  with a path and the other snapshot, and one command, `holdfast restore pair`. It returns both directories
  and the `diff -u` command. plan-m2 said the UI would make two calls. That would put logic in the UI.
- **Restore's "if a file already exists"** offers restic's own `--overwrite` choices (`always`,
  `if-changed`, `if-newer`, `never`), for in-place restores. The default is restic's own default.

**Q12. R103 and the spread.** Spec §4.5 says R103 warns when the worst-case data loss is more than a day, and
§4.1 makes that the longest gap plus the spread. So every daily job with a spread warns: daily at 02:00 with
20 minutes of spread can lose 24 h 20 min. M3 does exactly that. The mockup warns only when the interval itself
is over a day.
- *Proposal:* R103 compares the longest gap with a day, as the mockup does. Its message still gives the
  worst-case loss with the spread. Daily jobs no longer warn, and weekly ones still do.
- *The alternative:* keep it as it is; then the default job in Q11 has no spread, or warns.
