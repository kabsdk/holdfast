# Holdfast (proof of concept)

Holdfast is a control plane for Linux backups. **restic** moves and stores file data, **pgBackRest**
(not yet in this PoC) handles PostgreSQL, and **Prometheus** watches it all. Holdfast configures,
coordinates, verifies and reports. It never re-implements a storage format, so every repository stays
restorable with the plain `restic` binary if Holdfast is gone.

"Holdfast" is a working name.

## What this PoC proves

| Test (`test/e2e`) | What it shows |
| --- | --- |
| `TestRestoreOnAgentPreservesMetadata` | A server-queued restore runs **on the agent** and keeps owner, mode (setuid/setgid/sticky), ns mtimes, ACLs (access + default), user/trusted xattrs, SELinux label xattr, hard links, symlinks, FIFOs, odd file names and sparse holes |
| `TestAgentKeepsBackingUpWhileServerIsDown` | SIGKILL the server: the agent keeps backing up from cached config, spools results, delivers all of them on reconnect; server maintenance (forget/prune/check subset) runs via the API |
| `TestKilledBackupIsReportedAndRecovered` | SIGKILL the agent mid-upload: next start reports `interrupted`, cleans the stale restic lock, picks up a changed config, succeeds |
| `TestRetentionPolicyMatchesModel` | 240 snapshots over 180 days created with an injected clock (`restic --time`); forget result equals an independent model; full `check --read-data` after prune |
| `TestStalenessAndIndependentStorageWitness` | Stopped agent trips the staleness rule; the storage exporter agrees with the server when healthy and exposes a 2-hour disagreement when the newest snapshot is missing from storage |

## Layout

```
cmd/holdfast-server      control plane: desired state, enrollment, per-host repos + keys, restore jobs, maintenance, /metrics
cmd/holdfast-agent       runs on each host as root: local schedule, restic, spool, restore
cmd/holdfast-freshness   independent storage witness: newest snapshot object per host, /metrics
internal/restic          explicit restic CLI wrapper (password via file, exit-code 3 = warning, stale-only unlock)
internal/proto           agent <-> server protocol (HTTPS long-poll; each poll is a heartbeat)
deploy/prometheus        alert rules (stale, failed, offline, storage stale, witness disagreement, watchdog)
deploy/systemd           agent unit
deploy/examples          server.json (desired state) and agent.json
test/e2e                 end-to-end tests with real restic and real process kills
```

## Run the tests

Requirements: Go 1.24+, restic 0.18+, Linux, root (ownership, ACL and `trusted.*` tests). The code is
standard library only, with no third-party Go modules.

```sh
export HOLDFAST_RESTIC=/usr/bin/restic        # or have restic on PATH
export HOLDFAST_RESULTS=$PWD/test/results     # optional: JSON results per test
sudo -E go test ./test/e2e/ -count=1 -v -timeout 10m
```

## Try it by hand

```sh
go build -o bin/ ./cmd/...
bin/holdfast-server -config deploy/examples/server.json &
sudo bin/holdfast-agent -config deploy/examples/agent.json &
curl -s -H 'Authorization: Bearer CHANGE-ME' localhost:8480/api/v1/hosts | jq
curl -s localhost:8480/metrics
bin/holdfast-freshness -repos /srv/holdfast/repos -once
bin/holdfast-server -config deploy/examples/server.json -escrow-export /secure/holdfast-keys.json
```

## Not production-ready: known PoC shortcuts

- Plain HTTP in the PoC; production terminates TLS (and should pin the server CA in the agent).
- Server state is a JSON file and repository passwords are stored in plaintext (0600). Production: PostgreSQL with envelope encryption, plus an offline escrow bundle.
- Local-filesystem repositories only. The Ceph RGW backend (per-host S3 user, bucket policy, versioning) and the S3 lister in the freshness exporter are not built yet.
- Interval schedules only (no cron, no jitter, no blackout windows).
- No pgBackRest integration yet.
- No UI, LDAP/OIDC or RBAC. The admin API uses a single bearer token.
- One server; no leader election or standby.
