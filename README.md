# Holdfast

A free, open-source control plane for restic backups of Linux hosts (Ubuntu, RHEL, Oracle Linux). It
configures, schedules, monitors and restores restic backups, and it proves they happened: the server
knows what the agents report, and an independent storage witness checks the bucket.

- What it does: [docs/spec.md](docs/spec.md)
- Why, decision by decision: [docs/decisions/](docs/decisions/)
- What is not decided yet: [docs/open-questions.md](docs/open-questions.md)
- The current milestone: [docs/plan-m1.md](docs/plan-m1.md)
- Working rules for contributors and agents: [CLAUDE.md](CLAUDE.md)

## Layout

```
cmd/holdfast-server      control plane: config revisions in PostgreSQL, enrollment, /api/v1, agent protocol, metrics
cmd/holdfast-agent       runs on each host as root: local schedule, restic, event spool
cmd/holdfast             CLI, a thin client of /api/v1
cmd/holdfast-freshness   storage witness: newest snapshot object per host, read from the bucket
internal/                the packages behind them
deploy/                  Prometheus rules and their tests, systemd units, example configuration
test/e2e/                end-to-end tests: real restic, PostgreSQL, S3 and process kills
poc/                     the proof of concept, kept as a reference
mockup/                  UI mockups
```

## Build and test

Go 1.26, Docker, Linux (WSL2 works).

```sh
make build          # bin/holdfast-server, bin/holdfast-agent, bin/holdfast, bin/holdfast-freshness
make pg-up test     # unit and integration tests (a throwaway PostgreSQL in Docker)
make e2e            # the end-to-end suite: restic 0.19.1, MinIO, PostgreSQL; runs as root in a container
make rules-test     # promtool unit tests for the alert rules
```

## Licence

Apache License 2.0: see [LICENSE](LICENSE) and [NOTICE](NOTICE) (decision 043). Each package also carries the
licences of the third-party software it contains, in `/usr/share/doc/<package>/`.
