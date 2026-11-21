# Plan for M1: agent and server core

Status: development started on 2026-11-21, at your instruction ("we can start development").

## 0. What development started on

**Answered**
- **Q2: how config is changed before M4.** You chose API first (decision 028): the UI and the CLI are thin
  clients of the HTTP API.
  - M1 serves the same `/api/v1` on a root-only Unix socket on the server, and the actor is
    `local:<unix user>`, taken from the socket's peer credentials.
  - M4 adds the HTTPS listener with OIDC. The CLI talks to either.
- **Q4, its first question: who makes the hosts' S3 keys.** You chose: the admin, with Holdfast testing
  the storage contract, and one key per host (decision 029, [storage.md](storage.md)).
  - `holdfast host add --s3-key-file` takes the key, and `holdfast host set-key` replaces it. Both are
    API endpoints first.
  - A key that fails the contract is refused, and a host whose storage fails cannot enroll.
  - The optional provisioner command stays, for generated keys.
- **The architecture review (decision 030).** You accepted its recommendations, except the guard against
  snapshots dated in the future. The scope rule: Holdfast manages restic, and does not fix restic. Seven
  fixes to what is built belong to M1. They are listed under "Remaining for M1" below.
- **Q16: repository housekeeping.**
  - Git is initialised. There is no remote yet, so nothing is pushed.
  - The mockups are in `mockup/`.
  - `holdfast-poc.zip` is left in place but ignored by Git.

**Working assumptions.** Each of these is the proposal in §15, with one exception: for Q3 the plan made no
proposal. Each sits behind an interface where that matters, so it can change. **Say if any is wrong.**

| Question | Working assumption |
| --- | --- |
| Q1 (done) | No forget or prune code in M1. Maintenance is `check` only. Fidelity is proven by a plain `restic restore` using the escrow credentials. The retention test moves to M3. |
| Q3 (agent auth) | **Bearer token, stored as a hash, behind an `agentauth` interface,** so a client certificate from a Holdfast CA (OQ3) can replace it. This is a stand-in. It does not decide OQ3. |
| Q4 (the rest) | There is one maintenance key, for the whole fleet. The lifecycle rule for noncurrent versions, and its expiry, are still open. [storage.md](storage.md) asks the admin to set one. |
| Q5 (credentials) | Encrypted at rest with a root-only key file. Escrow export is CLI-only on the server. The bundle format is still open. |
| Q6 (time zone) | An IANA zone per schedule, defaulting to the fleet's zone. A time that does not exist runs at the next valid minute. A repeated time runs once. |
| Q7 (runs) | The first backup runs right after enrollment. A missed slot catches up once. One restic process per host. A slot whose job is still running is skipped and reported. No retry, until you decide. |
| Q8 (drift) | In sync when the sha256 of the applied host config matches the desired one. A save from a stale base revision is refused. Role bindings are part of the revision. |
| Q9 (FQDN) | Lower-case letters, digits and hyphens, at least two labels. Re-enrollment keeps the repository. A mismatch with `hostname -f` is a warning, not a refusal. |
| Q10 (restic) | Pin restic 0.19.1. The agent reports its version. Bundling comes with packaging. |
| Q11 (extra options) | None. |
| Q12 (tags and job IDs) | Tags are `job=<id>` only. The job ID is an immutable slug. |
| Q13 (built-in excludes) | The restic cache and the agent's state directory are always excluded. Database directories are handled in profiles (M5). |
| Q14 (stack) | Go 1.26, module `github.com/kabsdk/holdfast`, `pgx/v5`, `go.yaml.in/yaml/v3`, `minio-go/v7`, `cobra`. |
| Q15 (CI S3) | MinIO pinned by digest for e2e. The RGW storage-isolation test runs in the lab. **Update:** MinIO's images have left Docker Hub and quay.io entirely, so `make e2e` uses Chainguard's from-source rebuilds (`cgr.dev/chainguard/minio` and `minio-client`), pinned by digest. |

### Progress (2026-11-21, branch `m1`)

| Done and tested | Tests |
| --- | --- |
| Schedule evaluator, including time zones and daylight saving; config document; structural validation | unit |
| restic wrapper (no forget or prune), agent protocol types | unit, with a fake restic |
| PostgreSQL store: append-only revisions, `host_desired`, enrollment codes, events stored once, audit | integration, real PostgreSQL 17 |
| Server: API-first `/api/v1` on a root-only socket, agent protocol, enrollment, long-poll wake-up through LISTEN/NOTIFY, drift, metrics, check-only maintenance | integration |
| Agent: schedule and spread, catch-up once, skip when overlapping, spool, interrupted runs, commands run once | in-process against the server |
| CLI (`holdfast`) as a thin API client; storage witness; alert rules | promtool |
| e2e: fidelity with escrow, server outage, storage isolation, witness | real restic 0.19.1, MinIO, PostgreSQL |
| e2e: backup killed mid-upload and recovered (the PoC's test 3, using an S3 throttling proxy); exactly-once delivery with dropped answers, and runs on their schedule slots; history, diff, stale base, drift, `holdfast check`, audit, no credential leaks; enrollment refusals and expiry; TLS with certificate pinning | same |
| The repository check on the agent: once a day after a backup, `holdfast check` on request, timeouts (§10, decision 030). e2e: restic's check lock is exclusive | unit, integration, e2e: **all 11 e2e tests pass** |
| The storage contract (decision 029): admin-made keys, at `host add` and `host set-key`; the contract test (B001–B008) at host add, set-key, enrollment and in the daily check; a metric and an alert for a failed check. e2e: refused keys; enrollment refused while the policy is loose; restic ignores the probe objects; set-key reaches the agent; the B003 ownership case | unit, integration, e2e, promtool |

**Found and fixed along the way**
- The metric label `job` collides with Prometheus's own scrape label, so it is now `job_id`. The PoC had the same bug.
- `--retry-lock` must come before the restic subcommand. Placed after `--`, restic would have backed it up as a path.
- An agent killed during its very first backup would never have run that job again.
- The daily check ran every host on one fixed timer, not in each host's quiet stretch as §10 promised.
  Because check's lock is exclusive, it could have made scheduled backups fail.

**The lab** (`lab/`, in the style of Clusterlane).
- `lab/lab up m1` brings up five VMs:
  - s3a: Ceph Squid 19.2.3 RGW, with MicroCeph;
  - hfsrv: AlmaLinux 9, with PostgreSQL;
  - three agent hosts: Ubuntu 24.04, AlmaLinux 9 and Oracle Linux 9.
- A second `up` changes nothing. `lab/README.md` explains how to use it.
- dc-b and the vault come in M6.

**What real RGW taught, and MinIO could not.**
- On RGW an object belongs to the user who uploaded it. With plain users, the maintenance key gets 403
  on every host's data, so `restic check` fails. Forget and prune would fail the same way.
- MinIO has no ownership per object, so the e2e suite could not show this. restic cannot send
  `bucket-owner-full-control`, and RGW answers 400 to `BucketOwnerEnforced`.
- Two set-ups work. In both, each host key stays limited to its own prefix, and to deletes under
  `locks/` only, as spec §2.3 requires.
  - **An RGW account** holding all of Holdfast's users. Objects then belong to the account, and IAM
    policies decide access. 27 PASS, 0 FAIL. The maintenance key ran `check --read-data` and
    `forget --prune`.
  - **Plain users**, plus a bucket-policy statement that gives the maintenance user `s3:*` on the bucket
    and its objects. 40 and 38 PASS, 0 FAIL.
- The vault's reader key (O10) needs the same treatment.
- Decision 029 follows from this. Holdfast does not create keys. It tests the contract, and B003 names
  this case, with the fix, in [storage.md](storage.md).
- Holdfast's own contract test, run against the lab's RGW (`TestS3ProberLive`, see `lab/README.md`):
  - plain users: exactly B003, AccessDenied;
  - plain users plus the bucket-policy grant: every check passes, including B005, B006 and B007.
  - That was B001–B008. In the documented layout, which `lab/rgw-documented-layout.sh` builds, all of
    B001–B011 pass.

**Decision 030's fixes: done and tested**
1. Agent TLS: the pinned certificate is the only root, the server's certificate must chain to it, and
   the hostname is checked. Covered by unit tests and by an e2e impostor.
2. Job status follows the order in which the server receives reports.
3. Storage-witness alerts with their own threshold, so they fire with the server down. Covered by
   promtool tests.
4. A narrower maintenance key, with B009–B011, checked against MinIO.
5. Enrollment checks the code before it touches storage.
6. A timeout on each backup and check, and SIGINT before SIGKILL.
7. The repository check runs on the agent, after a backup.

**The documented storage layout, on real Ceph RGW.** `lab/rgw-documented-layout.sh` rebuilds the lab
bucket as `docs/storage.md` describes it. Holdfast's contract test then passes all of B001–B011 on Ceph
19.2.3, with plain users and the bucket-policy grant.

**What the contract test taught on MinIO.** On MinIO, `s3:DeleteObject` also deletes object versions for
good. A delete grant broader than `locks/*` therefore fails B006 as well as B005.

**What the e2e tests taught**
- A SIGKILLed restic leaves its lock behind. `restic unlock` removes it only once the process is gone. On a host, systemd reaps the orphaned process. In a container, PID 1 must do it, so `make e2e` runs the container with `--init`.
- The first backup runs at enrollment, so the second can follow it within seconds, at the next slot. Tests that compare consecutive snapshots wait for the third.

Spec: [spec.md](spec.md). Working rules: [../CLAUDE.md](../CLAUDE.md). Undecided topics:
[open-questions.md](open-questions.md).

## 1. Goal and definition of done

**Goal.** M1 builds the core:
- agent and server;
- enrollment;
- config revisions in PostgreSQL;
- the agent's local schedule;
- backup to a per-host S3 repository;
- the event spool;
- the storage witness;
- Prometheus metrics and alert rules.

**Done when** the PoC's e2e tests pass on the real codebase. Every test runs against real restic 0.19.1 (Q10),
real PostgreSQL and a real S3 server (MinIO in CI).

> **Q1 (blocking)** asks how to read "the PoC's e2e tests pass". Three of the five PoC tests exercise
> behaviour that the milestones place in M2 and M3, or that the spec forbids. §13 holds the proposed test
> list.

---

## 2. Inputs reviewed

### 2.1 Where the PoC disagrees with the spec

I read every file in `poc/`: the code, the tests, the deploy files and the recorded results. The PoC
predates several decisions, and here is where it differs.

| # | PoC | Spec | Proposal for M1 |
| --- | --- | --- | --- |
| P1 | Desired state is a JSON file. A comment in `server.go` says "in production this lives in Git … the file stays the source of truth". | Revisions in PostgreSQL. No Git, no approvers (§3.2). | Revisions table (§6). The JSON file goes. |
| P2 | `HostConfig.Version` is an FNV hash of the host config, **including the password**. | The agent reports the revision it has applied (§2.1, §3.1). | The agent reports (revision, sha256 of the host config). Credentials are not part of the hash (§7). |
| P3 | The repository password travels inside `HostConfig` on every config push. | Credentials are never shown, and revisions are diffable. | A separate credentials payload with its own version, never in a revision (§7, §9). |
| P4 | Host = any name (`web01`). Repositories are local, at `repo_base/<name>`. | Identity is the FQDN, with an S3 prefix `repos/<fqdn>`. | FQDN validation (Q9), S3 repositories. |
| P5 | Tags are `holdfast` and `job:<id>`. | The tag is `job=<id>`. | `job=<id>` only (Q12). |
| P6 | Count-based retention (`--keep-daily 7 --keep-weekly 4 …`), and `TestRetentionPolicyMatchesModel` asserts it. | Time windows only, plus `--keep-last` (§4.2). | Retention is stored in M1 but nothing runs it. The test is rewritten for time windows in M3 (Q1). |
| P7 | `interval` and `retry` durations, and a job that never ran is due immediately. | Structured schedules plus spread. No retry (§4.1). | Structured schedules in the agent (§8). Retry is Q7. |
| P8 | `forget --prune` runs every maintenance cycle, with no protection. | 7-day high-water mark. Prune now is the only bypass (§3.4). | **No forget or prune code path in M1** (Q1). |
| P9 | Restore uses `--include` only, into any absolute target. | Exclusion model, default root-only target, in-place restore backs up first (§5.4). | M2. Whether M1 keeps a minimal restore is Q1. |
| P10 | Static enrollment code in `server.json`, single-use, no expiry. | Single-use, 15 minutes, created by Add host (§5.7). | As in the spec. |
| P11 | Plain HTTP. One admin bearer token. | HTTPS. OIDC and roles (M4). | TLS from M1. Admin access in M1 is Q2. |
| P12 | `-escrow-export` writes plaintext JSON. | Offline escrow bundle (§2.3). | Q5. |
| P13 | The README says PostgreSQL will be handled by **pgBackRest**. | restic only. A PostgreSQL-native engine later (not named). Database files excluded (§1.2, §1.3). | The PoC README stays as history in `poc/`. |
| P14 | An allow-list of raw `extra_args` (`--one-file-system`, `--limit-upload`, …). | No raw restic. Jobs have paths and excludes only. | No extra options in M1 (Q11). |
| P15 | The systemd unit sets `ProtectHome=read-only` and `PrivateTmp=true`. | In-place restore anywhere. Backups of any path. | Drop both. `ProtectHome=read-only` blocks in-place restores into `/home` and `/root`. `PrivateTmp=true` means a job that backs up `/tmp` or `/var/tmp` sees an empty private directory. |
| P16 | The witness lists a local directory. The metric is `expected_interval`, and the stale threshold is 2 × the interval. | The witness lists S3. Stale after 2 × the longest gap (§4.1). | S3 lister. The metric becomes `holdfast_job_longest_gap_seconds` (§11). |
| P17 | Maintenance runs a rotating `check --read-data-subset N/30` daily. | The check cadence is not specified. | Keep the PoC cadence (§10). |

### 2.2 Where the mockup disagrees with the spec

The mockup is `App.dc.html`, which appeared in the repository root during this review. The brief expects
it at `mockup/` (Q16). Its `support.js` is missing. Line numbers below refer to that file.

**Where it matches.** There is no rollback button ("deliberately no rollback", L920), no Git or approvers,
no credentials, no raw restic console, no count-based retention, no cron schedules, and no Kubernetes,
CloudNativePG, database-file or "watched only" backups. The check codes and severities match. So do the
restore defaults, the Prune now confirmation, the Add host steps, the snapshot list, the snapshot tabs,
the Restores tab and "Jobs with the old values".

**Where the mockup contradicts the spec.** I propose to treat all of these as mockup bugs to fix in M4,
unless you say the spec should change.

| # | Mockup | Spec |
| --- | --- | --- |
| U1 | The user is shown as "operator" (L81). Prune now (L186, L910) and in-place restore (L1448) are offered with no role check. | viewer, editor, admin. Prune now and in-place restore are admin only. |
| U2 | Drift never shows: every host is forced to `cfg: curRev.n` (L1217), even an agent that has been silent for 2 days. | Drift is detected and shown. |
| U3 | "Holdfast never shows file contents" (L676, L1843). | Off by default. `content_view` can turn it on. |
| U4 | "Keep \<longer\> retention" opens a prefilled editor that still needs a save (L1715). | One click. |
| U5 | The Prune now command leaves out `--keep-last` (`forgetArgs(pp.to)`, L1679). The job editor's command includes it (L1567). | The exact command. |
| U6 | R103 tests the longest gap only (`DI.min > 1440`, L1564). A daily schedule with a 20-minute spread does not trigger it. | Worst-case loss is gap + spread. |
| U7 | S002 uses a 20% margin (L1551). S003 adds a "5 min minimum" rule (L1547). | Neither is in the spec. If they are wanted, the spec should say so. |
| U8 | Bulk library update: one checkbox accepts all warnings together (L2018). | Warnings are accepted one by one. |
| U9 | Some snapshot IDs are plain text: recent runs (L211), the restore result (L752) and the find-a-file column (L2112). | Every snapshot ID is a link. |
| U10 | Jobs store where they came from (`protFrom`, L1100, L1200), and the revision diff prints `protection: <name>` (L1508). | Library items are copies, with no "based on" link. |
| U11 | Add host has an extra "First job: paths" field (L979), and the job ID is hard-coded as `system` (L1772). | Choose a profile. |
| U12 | "Restore version A of this file" actually restores version B (L737, L2153). | (mockup bug) |
| U13 | A `canRollback` flag is still computed but not shown (L1731). The library protection editor's markup is broken (L492–535). | (mockup leftovers) |

**What the mockup has and the spec does not.** For each one, should the spec adopt it?

| # | Mockup | Bears on |
| --- | --- | --- |
| G1 | Automatic retry after a failure: "Interrupted · retry 14:10" (L1157). | Q7: the spec has no retry. |
| G2 | One job at a time per host: "run one at a time" (L164), "starts after any running job" (L1291). | Q7 |
| G3 | "Host renamed" in the audit log (L1330). | Q9: the spec has no rename. |
| G4 | In-place restore offers a choice of 4 `--overwrite` modes (L822), tags the pre-restore backup `pre-restore` (L830), and always targets `/` (L1444). | spec §5.4 |
| G5 | A "Reason" field on restores, with reason columns in the audit log and in history (L838, L239, L1022). | spec §3.3 |
| G6 | An Alerts page with PAGE and TICKET severities, using the shipped alert names (L1003); a fleet dashboard, a "Needs attention" filter and pinned hosts (L39–49); and "Standby site B in sync" (L52). | spec §2 |
| G7 | "Agent reported" and "Bucket listing" shown side by side for each job (L177). The witness is per host, not per job. | L6 |
| G8 | A "What you get" panel: the number of restore points, the oldest restore point and a timeline (L385). | spec §4 |
| G9 | Library item delete, guarded when a profile uses the item (L1950); "Leave them as they are" (L438). | spec §4.4 |
| G10 | CLI commands `holdfast prune preview … --json` (L1703) and `holdfast find` (L2074). Prune now confirmation shows "About X GiB freed" (L1677). | spec §3.4, §5.2 |
| G11 | Mount coverage, "not covered · mounted 3 days ago", is computed but not shown (L1150, L1301). | Q13 |
| G12 | Retention presets use weeks, for example `weekly: [8, 'weeks']` (L1100). restic durations have no week unit. | L1 |
| G13 | Config history has a "Pending prunes" list (L922). Prunes that have already run show only in recent runs and the audit log. | OQ2. The mockup does not decide it. |

**What the spec has and the mockup does not:**
- removing a job or a host, and decommission;
- role bindings and any permission gating;
- `content_view`;
- a drift indicator;
- deleted IDs from scheduled prunes (only Prune now records them, L1710).

**Terminology.** The mockup says "Protection" or "policy" for a protection item, "Other directory" for
"another directory", "Back up now", and "Operator console" (L5). I propose to keep the spec's names and fix
the mockup's labels in M4.

---

## 3. Scope

**In M1**
- `holdfast-agent`, `holdfast-server`, `holdfast-freshness` and the `holdfast` CLI.
- Enrollment:
  - Add host, which creates a revision and a 15-minute single-use code;
  - the install command;
  - progress through waiting → agent connected → first backup done.
- Config revisions: immutable, numbered, with author and message. List, show, and diff any two.
- Drift detection, as applied revision versus desired.
- The agent's local scheduler, running the structured schedules of spec §4.1, with spread.
- Backups to S3 (Ceph RGW in production, MinIO in CI), one repository per host.
- The event spool on disk, with exactly-once accounting on the server.
- "Backup now" as a server-queued command.
- The storage witness, using an S3 lister with a list-only key.
- Prometheus metrics on the server and the witness. Updated alert rules with `promtool` tests.
- Audit log and host history for every action that M1 has.
- Maintenance limited to `check`, and only if Q1 allows it.

**Not in M1**
- Restore and the snapshot catalogue (M2), unless Q1 says otherwise.
- The checks engine, forget, prune, the 7-day protection and Prune now (M3).
- The HTTPS admin API, OIDC, roles and the UI (M4).
- The library and profiles (M5).
- Off-site copies, rebuilding Holdfast and failover (M6, after the open questions are decided).

**Untouched by M1**
- **Open questions OQ1, OQ2 and OQ4.**
- **OQ3**, with one exception: the agent's identity, which is Q3.
- **The `holdfast-meta` bucket.** M1 does not write to it.
- **Hash-chaining.** The audit log is a plain append-only table.

---

## 4. Packages

The module path is `github.com/kabsdk/holdfast` and the Go version is 1.26. Both are part of Q14.

```
cmd/
  holdfast-agent/        agent daemon; also `holdfast-agent enroll …`
  holdfast-server/       control plane
  holdfast-freshness/    storage witness
  holdfast/              CLI (M1: over the admin socket, Q2)
internal/
  config/                config document types, canonical JSON, sha256, structural validation, diff
  schedule/              schedule evaluator: slots in a zone, spread, next N, longest gap;
                         shared by the agent (when to run) and the server (derived values, metrics)
  engine/                Engine interface and Capabilities (spec §1.3)
  engine/restic/         the restic engine (M1: schedule/run, freshness, identity)
  restic/                restic CLI wrapper, ported from the PoC; adds S3 env from a file,
                         a version check and --retry-lock (Q7)
  proto/                 agent <-> server wire types, versioned (/agent/v1)
  agent/                 run loop, scheduler state, enrollment, poll and report client
  agent/spool/           durable on-disk queue for events and received commands (fsync + rename)
  server/                HTTP wiring, TLS, agent endpoints, admin socket endpoints
  server/store/          PostgreSQL access and embedded SQL migrations
  server/revisions/      save (optimistic, base = head), list, show, diff, per-host desired config
  server/enroll/         codes, enrollment, re-enrollment (Q9)
  server/creds/          credential generation, encryption at rest, escrow export (Q5)
  server/storage/        S3 provisioning interface; RGW implementation, plus a MinIO one for CI (Q4)
  server/maint/          leader gate (M1: always the leader) and the check scheduler
  server/metrics/        Prometheus exposition
  audit/                 audit-log writer; every action goes through it
  freshness/             S3 lister (production) and local lister (tests), exposition
deploy/
  prometheus/            holdfast-alerts.yml and promtool tests
  systemd/               units (agent without ProtectHome or PrivateTmp, see P15)
test/e2e/                harness and tests ported from poc/test/e2e, plus new ones (§13)
```

`poc/` is not changed.

---

## 5. Config document

A revision stores one canonical JSON document. M1 uses the fields below. Library fields are present but
empty until M5.

```json
{
  "schema": 1,
  "hosts": {
    "web01.example.com": {
      "jobs": {
        "system": {
          "engine": "restic",
          "paths": ["/etc", "/home", "/var/www"],
          "excludes": ["/home/*/.cache"],
          "schedule": {
            "daily": {"times": ["02:00"]},
            "spread": {"enabled": true, "minutes": 30}
          },
          "retention": {
            "within":  {"n": 7,  "unit": "days"},
            "hourly":  null,
            "daily":   {"n": 30, "unit": "days"},
            "weekly":  null,
            "monthly": {"n": 12, "unit": "months"},
            "yearly":  null,
            "keep_last": 3
          }
        }
      }
    }
  },
  "library": {"paths": {}, "protection": {}, "profiles": {}},
  "access":  {"bindings": [
    {"group": "holdfast-admins",  "role": "admin",  "hosts": ["*"]},
    {"group": "holdfast-editors", "role": "editor", "hosts": ["*"]},
    {"group": "holdfast-viewers", "role": "viewer", "hosts": ["*"]}
  ]}
}
```

**Schedule.** Exactly one of these three forms:
- `{"every": {"n": 4, "unit": "hours", "from": "01:00"}}`
- `{"daily": {"times": ["02:00", "14:00"]}}`
- `{"weekly": {"days": ["mon", "thu"], "at": "03:00"}}`

The time zone is not settled yet (Q6).

**Retention.** It is stored in M1 but nothing acts on it until M3. Units are hours, days, weeks, months and
years. They are rendered as restic durations (y/m/d/h), so a week becomes 7d (L1).

**Validation in M1 is structural only:**
- the types are right;
- exactly one schedule form is given;
- times are valid;
- paths are absolute;
- job IDs are slugs (Q12).

The R and S check codes arrive with the checks engine in M3.

**`access`.** The spec calls the default bindings "normal config" (§6.3), but §3.2 lists only hosts, jobs and
library. Q8 asks which is right.

---

## 6. PostgreSQL schema

Migrations are embedded SQL files with a version table. Every table that has to be append-only gets a
trigger that rejects UPDATE and DELETE.

```sql
CREATE FUNCTION holdfast_append_only() RETURNS trigger LANGUAGE plpgsql AS
$$ BEGIN RAISE EXCEPTION '% is append-only', TG_TABLE_NAME; END $$;

-- Config revisions: linear, immutable, never contain credentials.
-- A save names its base; it inserts number = base + 1, so a concurrent save from the same base
-- fails on the primary key and is refused (Q8).
CREATE TABLE revisions (
  number         bigint      PRIMARY KEY CHECK (number > 0),
  created_at     timestamptz NOT NULL DEFAULT now(),
  actor          text        NOT NULL,            -- M1: local:<unix user> (Q2); later the OIDC subject
  source         text        NOT NULL CHECK (source IN ('cli','api','ui','system')),
  message        text        NOT NULL CHECK (message <> ''),
  accepted_codes text[]      NOT NULL DEFAULT '{}',  -- filled from M3
  config         jsonb       NOT NULL,
  config_sha256  bytea       NOT NULL
);
CREATE TRIGGER revisions_append_only BEFORE UPDATE OR DELETE ON revisions
  FOR EACH ROW EXECUTE FUNCTION holdfast_append_only();

-- The desired config per host changes only in some revisions. Drift compares against the latest row.
CREATE TABLE host_desired (
  fqdn      text   NOT NULL,
  revision  bigint NOT NULL REFERENCES revisions(number),
  sha256    bytea,                                  -- NULL: the host left the config in this revision
  PRIMARY KEY (fqdn, revision)
);

-- Lifecycle state; the config itself lives in revisions.
CREATE TYPE host_state AS ENUM ('awaiting_enrollment', 'enrolled', 'retired');
CREATE TABLE hosts (
  fqdn             text        PRIMARY KEY CHECK (fqdn = lower(fqdn) AND fqdn !~ '\.$'),
  state            host_state  NOT NULL,
  added_revision   bigint      NOT NULL REFERENCES revisions(number),
  added_at         timestamptz NOT NULL,
  connected_at     timestamptz,                     -- progress: agent connected
  first_backup_at  timestamptz                      -- progress: first backup done
);

CREATE TABLE enrollment_codes (
  id           uuid        PRIMARY KEY,
  fqdn         text        NOT NULL REFERENCES hosts(fqdn),
  code_sha256  bytea       NOT NULL UNIQUE,
  created_at   timestamptz NOT NULL,
  expires_at   timestamptz NOT NULL,                -- created_at + 15 minutes
  used_at      timestamptz,
  revoked_at   timestamptz,                         -- a newer code was issued
  created_by   text        NOT NULL
);
CREATE UNIQUE INDEX enrollment_codes_one_open ON enrollment_codes (fqdn)
  WHERE used_at IS NULL AND revoked_at IS NULL;

CREATE TABLE agents (
  fqdn                 text        PRIMARY KEY REFERENCES hosts(fqdn),
  credential_kind      text        NOT NULL CHECK (credential_kind IN ('token','certificate')),  -- Q3
  credential_sha256    bytea       NOT NULL UNIQUE, -- token hash or certificate fingerprint
  enrolled_at          timestamptz NOT NULL,
  last_seen_at         timestamptz,
  agent_version        text,
  restic_version       text,
  os_id                text,                        -- from os-release: ubuntu, rhel, ol
  os_version           text,
  hostname_fqdn        text,                        -- what `hostname -f` says (Q9)
  timezone             text,                        -- the host's zone (Q6)
  clock_offset_ms      integer,
  applied_revision     bigint,
  applied_sha256       bytea,
  applied_at           timestamptz,
  credentials_version  integer
);

-- Never part of a revision; never returned by any API or CLI call. Encryption is Q5.
CREATE TABLE host_credentials (
  fqdn             text        PRIMARY KEY REFERENCES hosts(fqdn),
  version          integer     NOT NULL,
  repository       text        NOT NULL,           -- s3:https://<endpoint>/<bucket>/repos/<fqdn>
  repo_password    bytea       NOT NULL,           -- ciphertext
  s3_access_key_id text        NOT NULL,
  s3_secret        bytea       NOT NULL,           -- ciphertext
  created_at       timestamptz NOT NULL,
  escrow_export_id bigint                          -- the latest bundle that contains this version
);
CREATE TABLE escrow_exports (
  id             bigserial   PRIMARY KEY,
  created_at     timestamptz NOT NULL,
  actor          text        NOT NULL,
  host_count     integer     NOT NULL,
  bundle_sha256  bytea       NOT NULL
);

-- Terminal results of runs, delivered at least once and stored exactly once.
CREATE TABLE events (
  fqdn           text        NOT NULL REFERENCES hosts(fqdn),
  id             uuid        NOT NULL,             -- generated by the agent
  kind           text        NOT NULL,             -- backup (M1); restore (M2)
  job_id         text,
  command_id     uuid,
  status         text        NOT NULL CHECK (status IN ('success','warning','failed','interrupted')),  -- plus 'skipped' if Q7
  scheduled_for  timestamptz,                      -- the slot; NULL for backup now
  spread_delay_s integer,
  started_at     timestamptz NOT NULL,
  finished_at    timestamptz NOT NULL,
  revision       bigint,                           -- the config revision the agent ran
  snapshot_id    text,                             -- full 64-hex ID
  files_new      bigint, files_changed bigint, bytes_added bigint,
  total_files    bigint, total_bytes   bigint,
  restic_exit    integer,
  error          text,
  received_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (fqdn, id)
);
CREATE INDEX events_job ON events (fqdn, job_id, finished_at DESC);

-- Kept in the same transaction as the event insert (only when the insert was new).
CREATE TABLE job_status (
  fqdn text NOT NULL, job_id text NOT NULL,
  last_run_at timestamptz, last_status text, last_error text,
  last_success_at timestamptz, last_snapshot_id text,
  PRIMARY KEY (fqdn, job_id)
);
CREATE TABLE run_counters (                         -- holdfast_runs_total
  fqdn text NOT NULL, kind text NOT NULL, job_id text NOT NULL DEFAULT '', status text NOT NULL,
  count bigint NOT NULL,
  PRIMARY KEY (fqdn, kind, job_id, status)
);

-- Work queued for an agent. Re-sent until the agent acknowledges receipt; the agent deduplicates by id.
CREATE TABLE commands (
  id            uuid        PRIMARY KEY,
  fqdn          text        NOT NULL REFERENCES hosts(fqdn),
  kind          text        NOT NULL CHECK (kind IN ('backup_now')),  -- 'restore' in M2
  job_id        text,
  payload       jsonb       NOT NULL DEFAULT '{}',
  created_at    timestamptz NOT NULL,
  created_by    text        NOT NULL,
  acked_at      timestamptz,
  result_event  uuid
);

CREATE TABLE maintenance_runs (
  id           bigserial   PRIMARY KEY,
  fqdn         text        NOT NULL REFERENCES hosts(fqdn),
  kind         text        NOT NULL CHECK (kind IN ('check')),  -- forget/prune arrive in M3
  subset       text,
  started_at   timestamptz NOT NULL,
  finished_at  timestamptz,
  status       text,
  error        text,
  node         text        NOT NULL                -- which server ran it (for M6)
);

-- Every action. Host history = audit_log for the host, merged with that host's events.
-- Plain append-only; hash-chaining waits for OQ3.
CREATE TABLE audit_log (
  id        bigserial   PRIMARY KEY,
  at        timestamptz NOT NULL DEFAULT now(),
  actor     text        NOT NULL,
  source    text        NOT NULL,
  action    text        NOT NULL,     -- host.add, enrollment.code_issued, agent.enrolled, revision.saved,
                                      -- backup.run_now, escrow.exported, server.reloaded, ...
  fqdn      text,
  job_id    text,
  revision  bigint,
  detail    jsonb       NOT NULL DEFAULT '{}'   -- never credentials
);
CREATE TRIGGER audit_log_append_only BEFORE UPDATE OR DELETE ON audit_log
  FOR EACH ROW EXECUTE FUNCTION holdfast_append_only();
```

**Waking long polls.** Saving a revision, queuing a command and changing credentials each issue
`NOTIFY holdfast_agent, '<fqdn>'`. Every server process LISTENs on that channel and wakes the long poll for
that host. This replaces the PoC's 200 ms polling loop, which held a mutex while it ran.

---

## 7. Agent ↔ server API

**Transport**
- HTTPS. The agent pins the server's CA, using the fingerprint given in the install command.
- Agent authentication is Q3: a bearer token, or a client certificate from a Holdfast CA.
- The server always takes the host identity from the credential. It never trusts a host named in a request
  body (as in the PoC).

### `POST /agent/v1/enroll`

```json
{"fqdn": "web01.example.com", "code": "…",
 "facts": {"hostname_fqdn": "web01.example.com", "os_id": "rhel", "os_version": "9.6",
           "agent_version": "0.1.0", "restic_version": "0.19.1", "timezone": "Europe/Copenhagen"},
 "csr": "…only if Q3 = certificate…"}
```

| Response | Meaning |
| --- | --- |
| `200 {"token": "…"}` or `{"certificate": "…"}` | Enrolled. |
| `403` | The code is invalid, expired, used or revoked. |
| `409` | The FQDN does not match `hostname -f` (if Q9 says it must). |

### `POST /agent/v1/poll`

The poll is a long poll of at most 30 s, and every call is also a heartbeat.

```json
{"applied": {"revision": 41, "sha256": "…"},
 "credentials_version": 1,
 "acked_commands": ["…"],
 "running": {"kind": "backup", "job_id": "system", "since": "2026-11-21T02:07:12Z"},
 "facts": {"agent_version": "…", "restic_version": "…", "os_id": "…", "os_version": "…",
           "hostname_fqdn": "…", "timezone": "…", "now": "2026-11-21T02:10:00.123Z"}}
```

The server answers at once if the desired config, the credentials version or the pending commands differ
from what the agent has. Otherwise it holds the request until something changes or the timeout passes.

```json
{"config": {"fqdn": "web01.example.com", "revision": 42, "sha256": "…",
            "repository": "s3:https://rgw.example.internal/holdfast/repos/web01.example.com",
            "jobs": [{"id": "system", "engine": "restic",
                      "paths": ["/etc", "/home"], "excludes": ["/home/*/.cache"],
                      "schedule": {"daily": {"times": ["02:00"]},
                                   "spread": {"enabled": true, "minutes": 30}}}]},
 "credentials": {"version": 2, "repo_password": "…",
                 "s3_access_key_id": "…", "s3_secret_access_key": "…"},
 "commands": [{"id": "…", "kind": "backup_now", "job_id": "system"}],
 "server_time": "2026-11-21T02:10:00.140Z"}
```

- `config` and `credentials` are present only when they changed.
- `sha256` is computed over the canonical JSON of `config` without the `sha256` field.
- Retention is not sent, because agents never forget (decision 007).
- `revision` is the revision in which this host's desired config last changed. A revision that does not
  touch this host changes nothing here, so it causes no drift.

### `POST /agent/v1/events`

```json
{"events": [{"id": "…", "kind": "backup", "job_id": "system", "status": "success",
             "scheduled_for": "…", "spread_delay_s": 812, "started_at": "…", "finished_at": "…",
             "revision": 42, "snapshot_id": "…64 hex…",
             "files_new": 12, "files_changed": 3, "bytes_added": 52311, "total_files": 40211,
             "total_bytes": 1830211231, "restic_exit": 0, "error": ""}]}
```

The response is `{"acked": ["…"]}`.
- The endpoint is idempotent: a duplicate is acknowledged again and not counted twice.
- The agent deletes a spooled event only after it has been acknowledged.

---

## 8. Agent: local state and run loop

### Files on the host

| Path | Holds |
| --- | --- |
| `/etc/holdfast/agent.yaml` | Bootstrap config, written by the install command: server URL, FQDN, server CA fingerprint. The enrollment code is never written to a file. It is passed once to `holdfast-agent enroll`. |
| `/etc/holdfast/credentials/` (0700, files 0600) | `repo.pass` and `s3.env`. Keeping them under `/etc/holdfast/` puts them under the spec's built-in content deny. Whether backups exclude them is Q13. |
| `/var/lib/holdfast-agent/` (0700) | `identity/`, `config.json` (with its sha256, re-checked on load), `schedule.json`, `spool/`, `commands/`, `running.json`, `cache/` (restic cache). |

### Run loop

The run loop keeps the PoC's behaviour, which is proven, and changes the schedule:
- **Order.** Queued commands come first, then due jobs.
- **Run marker.** `running.json` is written before restic starts and removed after the event has been
  spooled.
- **Interrupted runs.** If a marker is still there at startup, the agent reports `interrupted`, then runs
  `restic unlock` (stale locks only).
- **Exit codes.** 0 is `success`. 3 is `warning` (a snapshot was written but some files were unreadable).
  Anything else is `failed`. A run that never finishes is reported as `interrupted` at the next start.
- **Slots.** The scheduler computes slots in the schedule's time zone (Q6). For each slot it draws the
  spread delay once and saves it in `schedule.json`, so a restart does not re-draw it.
- **Open semantics.** Missed slots, the first run, overlapping runs, retry, and coexisting with maintenance
  locks are all Q7.

### restic invocation

```
restic backup --json --host <fqdn> --tag job=<id> [--exclude <pattern>]... [--retry-lock <d>] -- <paths>
```

| Environment variable | Value |
| --- | --- |
| `RESTIC_REPOSITORY` | the S3 repository URL |
| `RESTIC_PASSWORD_FILE` | `/etc/holdfast/credentials/repo.pass` |
| `RESTIC_CACHE_DIR` | `/var/lib/holdfast-agent/cache` |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | read from `s3.env` |

These variables are set only in restic's environment. The agent never inherits `RESTIC_*` or `AWS_*` from
its own parent process.

---

## 9. Storage and credentials

**Bucket layout.** One bucket, named in `server.yaml`. Each host's repository is under `repos/<fqdn>/`.

**S3 identities**

| Identity | Can do |
| --- | --- |
| Host key | `Get`, `Put` and `List` on its own prefix. `Delete` only on `repos/<fqdn>/locks/*`. Nothing on other prefixes. |
| Maintenance identity | Delete outside `locks/`, for prune in M3. Whether there is one identity or one per host is Q4. |
| Witness key | `ListBucket` on `repos/` only. |

**Startup checks.** When the server starts, it reads back the bucket's versioning setting and its policy,
and exports them as metrics (§11). It does not modify them silently.

**Enrollment, in one transaction:**
1. Provision the host key and its policy, if Holdfast provisions keys (Q4).
2. Generate the repository password: 32 random bytes, hex-encoded.
3. Run `restic init` as the maintenance identity.
4. Store the credentials, encrypted (Q5).
5. Mark the host as "not in escrow yet".
6. Write to the audit log.

**Re-enrollment** issues a new code for an existing FQDN. It keeps the repository and the password and
rotates only the agent's identity (Q9).

**Proposed additions to `server.yaml`.** The spec defines only `auth.*` and `content_view.*`, so these keys
are new:

```yaml
listen: 0.0.0.0:8443
tls: {cert_file: /etc/holdfast/tls/server.crt, key_file: /etc/holdfast/tls/server.key}
database: {dsn_file: /etc/holdfast/db.dsn}
storage:
  endpoint: https://rgw.example.internal
  bucket: holdfast
  region: default
  admin: {…}               # only if Holdfast provisions keys (Q4)
credentials:
  key_file: /etc/holdfast/credentials.key   # Q5
maintenance:
  check: daily             # or off; see §10
  check_cycle_days: 30
```

---

## 10. Maintenance in M1

- **No forget and no prune code path exists in M1.** The 7-day protection lands in M3, and CLAUDE.md
  forbids any destructive path that bypasses it.
- **`check` only, if Q1 keeps it.** `restic check --read-data-subset n/30`, with n rotating (as in the PoC).
- **The agent runs the check (decision 030).**
  - Once a day, right after a backup, inside the agent's one-restic-at-a-time loop, with the host's own
    key. It can never meet the host's own backups.
  - `holdfast check` tests every host's storage contract at once, and queues a `check_now` command for
    each agent. The result arrives as an event of kind `check`.
  - `maintenance.check: daily` (the default) sends the daily check to the agents. `off` leaves only
    `holdfast check`.
  - The server tests each host's storage contract once a day, and runs restic against a repository only
    to initialise it, and in M3 to prune it.
  - A backup or a check that runs past its timeout is stopped and reported as failed. The timeout is the
    schedule's longest gap, and at least an hour. A check's timeout is the shortest of the host's.
- **This replaced an earlier version from the same day.** That version had the server check each host
  in the middle of the day's longest quiet stretch, and recover an interrupted check at start-up. It still
  collided with long backups, because it planned around when runs start, not how long they last. The
  quiet-window function stays in `internal/schedule`, for placing prunes in M3.
- **`check` takes an exclusive lock. This is now measured on 0.19.1, not "reported".**
  `TestCheckLockIsExclusive` asserts it against MinIO with Holdfast's own identities. While check holds
  its lock:
  - the lock file says `"exclusive": true`;
  - a backup without `--retry-lock` fails at once with exit 11;
  - `snapshots` fails the same way, and `snapshots --no-lock` works;
  - `restic unlock` leaves the live lock alone;
  - a backup with `--retry-lock` waits and then succeeds.

  A check that meets a running backup also fails with exit 11. That is why the check runs on the agent.
- **restic is stopped gently.** When a run is cancelled or times out, restic gets SIGINT first, so it
  removes its own lock. It is killed only if it has not exited within a minute.
- **The leader gate is a stub.** It always says "leader" in M1. The VIP logic comes in M6.
- **For M2.** The snapshot catalogue must read with `--no-lock`. Otherwise every listing fails while a
  check runs, and every listing would also take a lock that delays backups.

---

## 11. Prometheus

### Server metrics

| Metric | Labels | Note |
| --- | --- | --- |
| `holdfast_server_up` | | |
| `holdfast_server_info` | `version` | |
| `holdfast_config_head_revision` | | |
| `holdfast_host_state` | `host`, `state` | 1 for the current state. |
| `holdfast_host_first_backup_timestamp_seconds` | `host` | |
| `holdfast_agent_last_seen_timestamp_seconds` | `host` | |
| `holdfast_agent_info` | `host`, `agent_version`, `restic_version`, `os_id`, `os_version` | |
| `holdfast_agent_applied_revision` | `host` | |
| `holdfast_agent_config_in_sync` | `host` | 1 or 0, per the drift rule (Q8). |
| `holdfast_agent_clock_offset_seconds` | `host` | |
| `holdfast_job_last_success_timestamp_seconds` | `host`, `job` | Success or warning. |
| `holdfast_job_last_status` | `host`, `job`, `status` | |
| `holdfast_job_longest_gap_seconds` | `host`, `job` | Replaces `expected_interval`. |
| `holdfast_runs_total` | `host`, `kind`, `job`, `status` | |
| `holdfast_maintenance_last_success_timestamp_seconds` | `host`, `kind` | |
| `holdfast_storage_versioning_enabled` | | Read back at startup and hourly. |
| `holdfast_storage_policy_ok` | | Read back at startup and hourly. |
| `holdfast_escrow_missing_hosts` | | Hosts whose current credentials are in no escrow bundle (Q5). |

### Witness metrics

The PoC's three metrics are kept:
- `holdfast_storage_scrape_success`
- `holdfast_storage_newest_snapshot_timestamp_seconds{host}`
- `holdfast_storage_snapshot_objects{host}`

They are now read with `ListObjectsV2` on `repos/<fqdn>/snapshots/`, using `LastModified`.

### Alert rules

**Changed from the PoC**
- **`HoldfastBackupStale`** and **`HoldfastStorageStale`** use `2 * holdfast_job_longest_gap_seconds`.

**New**
- **`HoldfastStorageMissing`.** The server expects a repository for a host that has had its first backup,
  but storage has none. Today, a vanished prefix stops the witness's series, so no alert can fire.
- **`HoldfastConfigDrift`.** The agent has not been in sync for 30 minutes.
- **`HoldfastAgentClockSkew`.** The agent's clock is off by more than 60 s.
- **`HoldfastStorageUnprotected`.** Versioning is off or the bucket policy is wrong.
- **`HoldfastEscrowOutdated`.** A host has had no escrow for 24 h (only if Q5 agrees).

**Tests.** `promtool check rules` and `promtool test rules` run in CI. They check the 1-hour and 2 × gap
thresholds with synthetic series, so the e2e tests do not have to wait an hour.

---

## 12. The admin CLI in M1

This assumes Q2 is answered "a root-only Unix socket". The command names are the ones M4 will expose over
HTTPS.

```
holdfast host add <fqdn> -m <msg>                 # revision + 15-min code + install command
holdfast host enroll-code <fqdn>                  # new code, revokes the open one (re-enrollment, Q9)
holdfast host list
holdfast host show <fqdn>                         # progress, applied revision, drift, jobs, last runs
holdfast job set <fqdn> <job> -m <msg>
      --path P... --exclude P...
      --every N{m|h} --from HH:MM | --daily HH:MM[,HH:MM...] | --weekly mon,thu --at HH:MM
      --spread N | --no-spread
      --keep-within D --keep-hourly D ... --keep-yearly D --keep-last N   # stored; inert until M3
holdfast job run <fqdn> <job>                     # backup now
holdfast revision list | show <n> | diff <a> <b>
holdfast audit list [--host <fqdn>]
holdfast escrow export --to <file>                # server root only; a parity exception (Q5)
```

`--keep-hourly` here means "one per hour for D", which maps to `--keep-within-hourly`. M3 will settle the
final flag names together with the checks.

---

## 13. End-to-end tests M1 must pass

### Harness

The harness is ported from `poc/test/e2e/harness_test.go`.
- It starts PostgreSQL and MinIO in Docker, one pair per test package, with a fresh database and bucket per
  test.
- It uses real restic 0.19.1, verified by sha256.
- It builds the real binaries and runs them as processes, so the tests can kill them with real signals.
- It adds an HTTP fault proxy between the agent and the server, and an S3 throttling proxy between restic
  and MinIO.

### Tests

| Test | Source | What it proves |
| --- | --- | --- |
| `TestEnrollment` | new | Add host by CLI creates revision N with its author and message. The code is single-use. It expires after 15 minutes, which the test forces by setting `expires_at` in PostgreSQL. A duplicate FQDN and an invalid FQDN are refused. Progress goes waiting → connected → first backup. The repository exists at `repos/<fqdn>`. |
| `TestBackupFidelity` | PoC 1 | The agent backs up the PoC fixture: owners, setuid/setgid/sticky, ns mtimes, access and default ACLs, `user.*`/`trusted.*` xattrs, the SELinux label, hard links, symlinks, FIFOs, odd names and sparse holes. Plain `restic restore`, using the **escrow bundle's** credentials, gives all 13 properties identical. This also proves the escrow bundle restores. (If Q1 moves agent restore into M1, the restore goes through the agent instead.) |
| `TestAgentKeepsBackingUpWhileControlPlaneIsDown` | PoC 2 | SIGKILL the server **and** stop PostgreSQL. The agent keeps making snapshots on schedule. After the restart, the spool drains, and the number of events stored equals the number of snapshots in the repository: none lost, none counted twice. If Q1 keeps `check`, the leader runs it and exports the metric. |
| `TestKilledBackupIsReportedAndRecovered` | PoC 3 | Throttle S3 through the proxy (the PoC used `--limit-upload`, see Q11). SIGKILL the agent mid-upload, save a new revision while it is down, and start it again. It reports `interrupted`, removes only the stale lock, applies and reports the new revision, and succeeds. |
| `TestEventsDeliveredExactlyOnce` | new | The fault proxy drops the responses to `/agent/v1/events`. The agent re-sends. Each event is stored once, and `holdfast_runs_total` equals the number of snapshots. |
| `TestStalenessAndIndependentStorageWitness` | PoC 5 | The witness lists MinIO with a list-only key, and agrees with the server while healthy. The newest snapshot object is deleted with the admin key (versioned, so this creates a delete marker): the witness falls behind the server. A stopped agent trips the stale condition. The thresholds are covered by the `promtool` tests. |
| `TestRevisionsAndDrift` | new | Revisions are numbered and immutable: the database rejects UPDATE and DELETE. Any two can be diffed. A save from a stale base is refused. The agent reports its applied revision and sha. A revision that does not touch a host leaves it in sync. An agent that is offline during a change shows out of sync until it reconnects. |
| `TestScheduleSlots` | new | With an every-1-minute schedule and no spread, snapshots land on minute slots within tolerance. With spread, the delays are within [0, spread]. Unit tests cover zones, DST, the next 5 runs and the longest gap without waiting. |
| `TestStorageIsolation` | new | A host key can write its own prefix. It cannot list or read another host's prefix, and it cannot delete outside `locks/`. Versioning is on. The witness key can only list. |
| `TestCredentialsNeverLeak` | new | The repository password and the S3 secret are absent from revisions, the audit log, CLI output, both logs and both metrics endpoints. |
| `TestPrometheusRules` | new | `promtool check rules` and `promtool test rules` pass. |
| `TestCheckLockIsExclusive` | new | The restic behaviour behind §10, with Holdfast's own keys. Check's lock is exclusive. While check holds it, a backup fails with exit 11 at once, or waits with `--retry-lock`. Locking reads fail and `--no-lock` reads work. Unlock leaves the live lock. A check that meets a running backup fails with exit 11. |

### Moved out of M1

`TestRetentionPolicyMatchesModel` moves to M3. It will be rewritten for `--keep-within-*` and will assert
the keep-oldest behaviour for those flags (L1).

### Test duration

The spec's smallest schedule unit is a minute, so the outage and slot tests take minutes, where the PoC's
2-second intervals took seconds. I propose to accept this and run the tests in parallel, rather than add a
seconds-unit schedule that exists only for tests. This is part of Q1.

---

## 14. CI

- **Runner.** Linux with Docker and root (the fidelity test needs root).
- **restic.** 0.19.1 from the GitHub release, verified against a pinned sha256 (Q10).
- **PostgreSQL.** The production major version (Q14).
- **MinIO.** Pinned by image digest (Q15).
- **`promtool`.** The Prometheus release, pinned.
- **Jobs.**
  - `go vet` and the unit tests;
  - the e2e tests (`sudo -E go test ./test/e2e/ -count=1`);
  - `promtool`;
  - and, if Q15 agrees, a storage-policy job against real Ceph RGW.

---

## 15. Questions that block M1

**Q1. What "the PoC's e2e tests pass" means for M1.**
Three of the five PoC tests cover work that is outside M1 or forbidden by the spec:
- `TestRestoreOnAgentPreservesMetadata` needs restore via the agent, which is M2.
- `TestAgentKeepsBackingUpWhileServerIsDown` runs `forget --prune`, which is M3 and must not run before the
  7-day protection exists.
- `TestRetentionPolicyMatchesModel` tests count flags, which the spec forbids.

**Proposal:**
- **No forget or prune path in M1.**
- **Fidelity is proven** by a plain `restic restore` using the escrow credentials. Agent restore stays in M2.
- **Retention:** the test is rewritten for time windows in M3.
- **Maintenance in M1 is `check` only**, or none if you prefer.
- **Minute-granularity tests:** accept them, with no test-only seconds unit.

Confirm this, or tell me to pull a minimal agent restore into M1.

**Q2. How config is changed in M1, before the HTTPS API and OIDC (M4).**
Every action needs an actor in the audit log.

*Proposal:* the `holdfast` CLI talks to the server over a root-only Unix socket. The actor is
`local:<unix user>`, taken from the socket's peer credentials. The command names are the ones M4 will
expose (§12).

*Alternative:* build the HTTPS API with OIDC in M1, which brings part of M4 forward.

**Q3. Agent identity and authentication.**
The choice is a bearer token (the PoC's approach, stored as a hash) or a client certificate issued at
enrollment from a CSR, so that the key never leaves the host. A client certificate from a Holdfast CA is
part of **OQ3**, so I will not choose. The schema allows either (`credential_kind`).

**Q4. Provisioning S3 and the maintenance identity.**
- **Who creates each host's S3 key and its prefix-scoped policy?**
  - **Holdfast.** It would need an RGW admin-ops credential in `server.yaml`, and CI would need a MinIO
    admin implementation that only CI uses.
  - **An operator, beforehand.** That fits badly with the one-step Add host flow.
- **Is the maintenance identity one fleet-wide key with delete rights, or one per host?**
- **With versioning on, prune frees no space until noncurrent versions expire.** Who sets the lifecycle
  rule, and what is the expiry? It is also the window in which deleted data can still be recovered.

**Q5. Credentials at rest, and the escrow bundle.**
- **At rest.** What encrypts the credentials in PostgreSQL, and where does that key live? The proposal is a
  root-only key file on the server and on the standby.
- **Bundle format.** For example, encrypted to an `age` recipient whose private key is kept offline.
- **When a bundle is made.** By manual export, or automatically when a host is added. A host added after the
  last export cannot be recovered if PostgreSQL is lost. The proposal is the metric and the alert in §11.
- **The UI.** Escrow export cannot be in the UI ("credentials are never shown"). It is the first parity
  exception, alongside `server.yaml`. Agree?

**Q6. Time zone and daylight saving for schedules.**
The agent runs the schedule and the server derives the next 5 runs and the longest gap, so both must use the
same zone.
- **Which zone?** The host's local zone, one fleet-wide zone, or an IANA zone per schedule?
- **What happens to times that do not exist or that repeat?** For example, 02:30 on the spring-forward day
  and on the fall-back day.

*Proposal:* an IANA zone per schedule, defaulting to a fleet-wide default. A time that does not exist runs at
the next valid minute. A repeated time runs once.

**Q7. How runs behave.**
- **First run.** Does the first backup run immediately after enrollment? The "first backup done" progress
  step implies it, and the PoC does it.
- **Missed slots.** If the host or agent was down at a slot, does it catch up once on start (like
  `Persistent=true` in systemd), or wait for the next slot? *Proposal:* catch up once.
- **Overlap.** If a slot comes while the previous run is still going, is it skipped and recorded with a new
  `skipped` status, or queued? Do different jobs on one host run one at a time or in parallel? *Proposal:*
  one restic process per host, and a slot whose job is still running is skipped and reported.
- **Retry.** The PoC retries a failed run (`retry`), and so does the mockup ("Interrupted · retry 14:10",
  G1). The spec has no retry. Without it, a failed nightly backup waits 24 h, and the worst-case-loss figure
  is wrong. Add a structured retry, or none?
- **Coexisting with maintenance.** Prune takes an exclusive lock, and `check` reportedly does too. While
  either runs, backups fail with "locked". *Proposal:* backups use `--retry-lock`, the server schedules
  exclusive maintenance inside each host's longest gap, and a waiting backup shows "waiting for maintenance
  lock".

**Q8. What drift means, and concurrent saves.**
- **Drift.** *Proposal:* the agent reports the revision and sha256 of the host config it runs. It is in sync
  when that sha256 equals the host's current desired sha256. The agent also reports a local edit of its
  cached config. Does "drift" mean anything more? For example a different restic version, or snapshots in
  the repository that Holdfast did not make.
- **Concurrent saves.** *Proposal:* every save names its base revision, and if the head has moved, the save
  is refused with a diff. It is not merged automatically.
- **Role bindings.** Are they part of the revision? §6.3 says "normal config", but §3.2 lists only hosts,
  jobs and library.

**Q9. FQDN rules, re-enrollment and re-adding a host.**
- **Normalisation.** *Proposal:* lower-case, no trailing dot, letters, digits and hyphens only, at least two
  labels.
- **Matching the host.** Must the agent's `hostname -f` match the FQDN, with enrollment refused otherwise?
- **Re-enrollment.** A reinstalled host, or one that lost its agent state, needs a new code for the same
  FQDN that keeps its repository and password. It is the disaster-recovery path. Is it in M1?
- **A retired FQDN.** If a host whose repository was retired is added again, is the add refused, or does it
  re-attach to the retired repository?
- **Renaming a host.** The mockup's audit log has "Host renamed" (G3), but the spec has no rename. A rename
  changes the identity, the repository prefix and the `--host` value. Out of scope for now?

**Q10. The restic binary.**
Ubuntu and EPEL ship different restic versions, and both lag behind upstream. Behaviour the spec relies on
changes between versions: keep-oldest, the JSON formats, `restore --exclude` and `--overwrite`.

*Proposal:* ship a pinned restic with the agent package, for example `/usr/libexec/holdfast/restic`, and
have the agent refuse any other version. The agent reports its version in any case.

**Which version?** The PoC was tested on 0.18.1, but the current stable release is **0.19.1**
(2026-07-05). *Proposal:* pin 0.19.1 and re-run all five PoC tests on it
([research/engine-review.md](research/engine-review.md) §5).

**Q11. No extra backup options.**
The PoC allowed `--one-file-system`, `--exclude-caches`, `--limit-upload` and others. The spec has paths
and excludes only.

*Proposal:* no extra options in M1. The kill-mid-upload test gets its slow upload from an S3 throttling
proxy in the harness, not from a product option. Do any of these options belong in the product as typed
fields?

**Q12. Tags, job IDs and untagged snapshots.**
- **Tags.** *Proposal:* the only tag is `job=<id>`, with no `holdfast` tag.
- **Job IDs.** *Proposal:* the job ID is an immutable slug chosen at creation and unique per host, and
  renaming a job changes only its display name. A changed tag would split the job's retention group.
- **Untagged snapshots.** Snapshots can appear in a repository without a `job=` tag, for example when root on
  the host runs restic with the password file. Should Holdfast ignore them, show them, or flag them as
  drift? Holdfast's forget would never touch them.

**Q13. Built-in excludes.**
- **Database directories.** Is "e.g. `/var/lib/pgsql` is excluded" enforced in every job as a non-removable
  list (`/var/lib/pgsql`, `/var/lib/postgresql`, `/var/lib/mysql`, …), or only a default in profiles?
- **Holdfast's own files.** Should every job also exclude the restic cache, the spool and Holdfast's
  credential files?

**Q14. Dependencies, module path, Go and PostgreSQL versions.**
The PoC uses only the standard library. M1 needs a PostgreSQL driver, YAML and an S3 client.

*Proposal:*
- `jackc/pgx/v5`, `go.yaml.in/yaml/v3` and `minio-go/v7`, which handles listing against RGW and MinIO;
- `ceph/go-ceph/rgw/admin`, only if Q4 says Holdfast provisions keys;
- `spf13/cobra` for the CLI;
- a hand-written Prometheus exposition, as in the PoC, or `client_golang`.

Also to confirm:
- the module path `github.com/kabsdk/holdfast`;
- Go 1.26, which is the version installed here;
- which PostgreSQL major version production runs.

**Q15. S3 in CI.**
- **MinIO's status.** Its community edition has been source-only since October 2025, and its repository has
  been in maintenance mode since December 2025. Sources:
  [InfoQ](https://infoq.com/news/2025/12/minio-s3-api-alternatives/),
  [Chainguard](https://www.chainguard.dev/de-DE/unchained/secure-and-free-minio-chainguard-containers).
  The choices are to pin the last official image by digest, build it from source, or use a republished
  image.
- **More important: MinIO is not RGW.** Its policy engine is not RGW's, and "deletes only under `locks/`"
  is a security boundary. Tested only on MinIO, it is not tested where it runs.
- *Proposal:* MinIO, pinned, for everything, plus a `TestStorageIsolation` job against real Ceph RGW, at
  least nightly.

**Q16. Repository housekeeping.**
- **Git.** The directory is not a Git repository and has no remote, but the plan is "one milestone per PR".
  Should I run `git init`, and where is the remote?
- **License.** None is chosen yet.
- **`poc/`.** It was not in the repository. I unpacked it from `holdfast-poc.zip` in Downloads. A copy of
  that zip also appeared in the repository root. Should it stay?
- **The mockup.** It appeared as `App.dc.html` in the repository root, not at `mockup/App.dc.html`, and its
  `support.js` is missing. Should I move it to `mockup/`?
- **Mockup issues.** §2.2 lists them. Confirm that U1–U13 are mockup bugs to fix in M4, and say which of
  G1–G12 the spec should adopt.
- **Tests.** The plan ports `poc/test/e2e` into `test/e2e` and leaves `poc/` unchanged. Agree?

---

## 16. Questions for later milestones, raised now

These do not block M1, but they affect the spec, so it is better to settle them before their milestone.

**L1. Retention windows are measured from the newest snapshot in the group, not from now** (M3).
restic documents `--keep-within*` this way.
- **A job whose backups stop keeps its whole history.** This fits "removal never deletes data". It also means
  a retired job never ages out.
- **The anchor must be the same everywhere.** The pending-prune preview, "which rule keeps each" and R101
  must all use it.
- **restic durations have only y, m, d and h,** so "weeks" has to become days.
- **Keep-oldest is not yet verified for these flags.** The PoC verified it only for `--keep-yearly 2`, a
  count flag. It has to be verified for `--keep-within-*`.

Should the spec say all of this?

**L2. Prune now can race with a new backup** (M3).
If the command uses the policy flags, a backup that lands between the confirmation page and the run moves
the anchor. It can then delete snapshots that are not on the list the admin approved. Scheduled maintenance
under the high-water mark has the same problem, because the union of several policies cannot be written as
one set of flags.

*Proposal:* run `restic forget <exact IDs> --prune`, and show exactly that as "the exact command".

**L3. Deleted snapshot IDs and the catalogue** (M2 and M3).
"Deleted IDs stay findable" conflicts with "the catalogue is a cache rebuilt from the repositories"
(OQ3's leaning). A deleted ID cannot be rebuilt from the repository, so the record of a deletion has to be
durable state, not cache. This does not decide OQ3; it only constrains it.

**L4. Where Files, Changes and find run, and how a dead host is restored** (M2).
"Behave like restic on the host": is that about semantics or about location?
- **On the server.** Works for an offline or dead host, needs no file contents, and the server already holds
  the passwords.
- **On the agent.** Works only while the host is online.

*Proposal:* on the server. Restore always runs on the agent.

The spec also has no restore to a different host, so the only way to recover a dead host is to re-enroll
the same FQDN (Q9). Is that deliberate?

**L5. Which permission "another directory" needs** (M2 and M4).
A restore into an existing directory can overwrite files, which makes it in-place in effect. *Proposal:*
`restore.new_dir` covers only directories that Holdfast creates. An existing directory needs
`restore.in_place`.

**L6. The witness judges per host, not per job** (spec §2.1).
It has one repository per host and no password, so it can see only that some snapshot landed for the host.
A stale job next to a healthy job on the same host is caught only by the server's metrics. Should the spec
say so explicitly?

**L7. Wording** (spec §6.5).
"Every job's repository password file": the password belongs to the host, one per repository. Should this
read "each host's repository password file"?

**L8. Unassigned or unspecified**
- Which milestone gets `content_view`? None does today.
- Which permission covers Add host? `config.jobs`?
- Who can read the audit log?
- How long is the decommission grace period?

**L9. The "identity" capability** (spec §1.3).
What does it mean? I listed it without a definition.

**L10. High availability details** (M6).
- How does a server know it holds the VIP?
- What do the standby's metrics look like, so they do not raise false alerts?
- Is the replica promoted by hand or automatically?
