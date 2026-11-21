# Plan for M3: checks, retention, the 7-day protection, Prune now, Delete snapshot

Status: accepted, 2026-11-21. You left the questions in §6 to me ("You decide, continue"), so every proposal
stands as written. That includes Q3, which decides OQ2: there is a list of past prunes per host.

## 1. Goal and done when

**Goal.** Holdfast thins out old backups, safely:
- restic decides what each policy keeps (decision 030);
- nothing that a policy in force during the last 7 days would keep is ever removed (decision 015);
- an admin can bypass that with Prune now or Delete now, behind a confirmation (decisions 015 and 031).

The checks engine validates every config change, with stable codes (spec §4.5).

**Done when** the PoC's retention test passes on the real codebase, ported from
`poc/test/e2e/TestRetentionPolicyMatchesModel`: 180 days of snapshots made with `restic --time`, then
Holdfast's own retention. These e2e tests pass as well:
- a retention cut waits 7 days, and Keep longer undoes it;
- Prune now, with its confirmation;
- Delete snapshot, with its 7-day wait and Delete now;
- the checks and their codes;
- prunes placed away from backups.

## 2. Scope

| Feature | API | CLI |
| --- | --- | --- |
| Checks on every config save: errors block, warnings must be accepted (spec §4.5) | on the existing save endpoints, plus `POST /api/v1/check` to test a change without saving | `--accept R102,R104` on `job set` and `host add`; `holdfast job check` |
| Retention by restic, per job, with the 7-day union | daily server maintenance | |
| Pending prunes: what will go, and when; **Keep longer** | `GET /api/v1/hosts/{fqdn}/pending`; Keep longer is a normal revision | `holdfast pending <fqdn>`; `holdfast job keep-longer` |
| **Prune now** (admin): a preview, then a confirmation with the typed short name | `GET …/prune-now` (preview), `POST …/prune-now` | `holdfast prune-now <fqdn> [--confirm <name>]` |
| **Delete snapshot** (decision 031): a 7-day wait, cancel, **Delete now** | `POST/DELETE …/snapshots/{id}/delete` | `holdfast snapshot delete <fqdn> <id> [--now --confirm <name>]`, `--cancel` |
| When the data is really gone (decision 031) | in the snapshot and pending views | |

**Not in M3:** roles. M3 runs on the root-only socket, as M1 and M2 do, and `retention.cut` and
`snapshot.delete` are checked from M4.

## 3. Design in brief

**Retention, per job, once a day, on the server:**
1. Collect the job's retention policies that were in force during the last 7 days, from the config
   revisions.
2. For each one, run `restic forget --dry-run --json --tag job=<id> --group-by host` with its
   `--keep-within*` flags. restic says what that policy keeps.
3. Keep the union. Forget the rest by snapshot ID, then prune.

Snapshots that Holdfast did not make have no job tag, so they are never touched (decision 032).

**Pending prunes need no state of their own.** A snapshot is pending when only a policy older than the
current one keeps it. It becomes prunable 7 days after that policy stopped being in force. **Keep longer**
saves a new revision with the old retention, so the snapshot is kept again.

**Prune placement.**
- Prune takes an exclusive lock, so it runs in the middle of the host's longest quiet stretch, using the
  `QuietWindow` function kept from M1.
- A structural `restic check` runs first. If it fails, nothing is pruned, and the failure is reported.
- Deleted snapshots get `deleted_by_holdfast` tombstones, so search marks them **Deleted**, not
  disappeared.

**Delete snapshot (decision 031).**
- A pending deletion is a row with the snapshot, who asked, and when.
- The daily maintenance forgets it after 7 days.
- Cancel removes the row. Delete now forgets it at once.
- The space is freed at the next prune.

**"Really gone" dates.** The daily storage test already reads the lifecycle rule (B011). M3 keeps its
noncurrent-version days. The gone date is then the prune date plus those days.

## 4. Checks (spec §4.5)

| Code | How it is decided |
| --- | --- |
| R001–R006, S001, S003, I001 | From the job's schedule and retention fields. Unit-tested. |
| S002 (runs more often than they take) | From the p95 run time of the job's last 20 runs. No warning until there are 5 runs. |
| R101, R102, R103 | Estimated from the schedule and the windows. These are warnings, so an estimate is enough; the CLI shows them as "about". |
| R104 (the change cuts retention) | restic decides: `forget --dry-run` on the real repository, old policy against new. It is a cut if the new keep set is a strict subset. |

## 5. Tests

- e2e, ported from the PoC's retention test: 180 days of snapshots, then Holdfast's daily retention. The
  survivors are exactly what restic keeps, and a full `check --read-data` passes after the prune.
- e2e:
  - a cut keeps the extra snapshots for 7 days, using a controlled clock in the server;
  - Keep longer;
  - Prune now, refused without the typed name;
  - Delete snapshot, then cancel, then Delete now;
  - a manual snapshot (not Holdfast's) is never forgotten.
- Unit: each check code, with its boundary cases.
- Integration: prune placement uses the quiet window, and a failed pre-prune check blocks the prune.

## 6. Questions

**Q1. Grouping.** Retention forgets per job with `--group-by host`, ignoring paths.
- *Why:* an in-place restore's safety backup covers fewer paths. Grouped by paths, it would form its own
  group, and "keep the newest 3" would keep it for ever. Changing a job's paths would also split its
  history.
- *Proposal:* yes, `--group-by host`, with `--tag job=<id>`.

**Q2. Prune timing.** *Proposal:* once a day per host, after its forget, in the middle of the longest quiet
stretch, with a structural check first.

**Q3. OQ2: a list of past prunes.** This is an open question, so I'm asking.
- *Proposal:* yes, per host. Each entry shows when, scheduled or Prune now, who, how many snapshots were
  forgotten, and the bytes freed according to restic. It is built from the maintenance runs that M3
  records anyway.

**Q4. Estimated warnings.** *Proposal:* R101, R102 and R103 are estimates from the schedule and the windows,
shown as "about". R104 is exact, from restic.

**Q5. Prune now's scope.** Prune now forgets what the *current* policy would drop, ignoring the 7-day
union, then prunes. *Proposal:* yes, as decision 015 says. The preview shows exactly which snapshots, and
the restic command.

**Q6. A test clock.** The e2e tests need the server's clock to move 7 days.
- *Proposal:* a test-only setting, `HOLDFAST_TEST_NOW_OFFSET` in the server's environment. It is refused
  unless `holdfast-server` runs with `-test`, so a production server can never have it.
- *The alternative:* run those cases as integration tests only.
