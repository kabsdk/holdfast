# Holdfast

A free, open-source control plane for restic backups of Linux hosts (Ubuntu, RHEL, Oracle Linux). It
configures, schedules, monitors and restores restic backups, and it proves they happened: the server
knows what the agents report, and checks the bucket for each backup with restic, not through the agents.

- The documentation for people who run it: [website/index.html](website/index.html), the website's page
- What it does: [docs/spec.md](docs/spec.md)
- Why, decision by decision: [docs/decisions/](docs/decisions/)
- What is not decided yet: [docs/open-questions.md](docs/open-questions.md)
- The latest milestone: [docs/plan-m10.md](docs/plan-m10.md)
- Working rules for contributors and agents: [CLAUDE.md](CLAUDE.md)

## Layout

```
cmd/holdfast-server      control plane: config revisions in PostgreSQL, enrollment, /api/v1, the gateway, the storage
                         witness, alerts, metrics
cmd/holdfast-agent       runs on each host as root: local schedule, restic, event spool
cmd/holdfast             CLI, a thin client of /api/v1
internal/                the packages behind them
deploy/                  Prometheus rules and their tests, systemd units, example configuration
scripts/install.sh       the one-line installer
ansible/                 the Ansible collection kabsdk.holdfast: the installer as roles (decision 050)
test/e2e/                end-to-end tests: real restic, PostgreSQL, S3 and process kills
test/install/            the installer's test, on systemd containers
test/ansible/            the collection's test, on systemd containers
poc/                     the proof of concept, kept as a reference
mockup/                  UI mockups
```

## Build and test

Go 1.26, Docker, Linux (WSL2 works).

```sh
make build          # bin/holdfast-server, bin/holdfast-agent, bin/holdfast
make pg-up test     # unit and integration tests (a throwaway PostgreSQL in Docker)
make e2e            # the end-to-end suite: restic 0.19.1, MinIO, PostgreSQL; runs as root in a container
make rules-test     # promtool unit tests for the alert rules
make packages       # the .deb and .rpm packages, install.sh and SHA256SUMS in dist/: one release
make install-test   # install.sh as people run it, on systemd containers of each distribution, against MinIO
make ansible-test   # the Ansible collection, linted and run twice on the same kind of machines: the second changes nothing
```

A release is `scripts/release.sh <version>`: it builds the packages and the collection at that version, tags the
commit and publishes them as a GitHub release.

## Licence

Apache License 2.0: see [LICENSE](LICENSE) and [NOTICE](NOTICE) (decision 043). Each package also carries the
licences of the third-party software it contains, in `/usr/share/doc/<package>/`.
