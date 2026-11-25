# Contributing to Holdfast

Bug reports, fixes and ideas are welcome. For anything larger than a fix, open an issue first: say what you want to
change and why, and list what is unclear as questions. That plan lives in the issue, and is agreed before code is
written.

## The rules every change keeps

Holdfast manages restic with sane defaults for a small team and one server. It does not add protections restic itself
lacks, and it has no high availability by design. Within that, a change never breaks these:

- **The web UI, the API and the CLI are the same product.** Every option and action exists in all three, with the
  same names, checks and check codes. Logic and validation live behind the API; the UI and the CLI are thin clients.
- **No file contents leave a host through Holdfast**, only names and metadata. Restores run on the host's agent.
- **Every action is audited**, with who, when and any warnings accepted.
- **Nothing destructive skips the 7-day protection**, except an admin's Prune now, Delete now or rollback.
- **Hosts hold no S3 key**, and agents never run forget or prune.
- **No credentials** in the UI, in API or CLI output, in logs, in metrics or in config revisions; no password in
  argv; restic never inherits `RESTIC_*` or `AWS_*` from its parent.

[CLAUDE.md](CLAUDE.md) has the full list, and [docs/decisions/](docs/decisions/) the reasons.

## Building and testing

You need Linux (WSL2 works), Go 1.26 and Docker. The tests start everything else in containers.

```sh
make build          # bin/holdfast-server, bin/holdfast-agent, bin/holdfast
make pg-up test     # vet, and the unit and integration tests, against a throwaway PostgreSQL
make e2e            # the end-to-end suite: real restic, MinIO, PostgreSQL, an OIDC provider and a browser
make rules-test     # promtool tests for the Prometheus alert rules
make packages       # the .deb and .rpm packages, install.sh and SHA256SUMS in dist/
make install-test   # install.sh as people run it, on systemd containers of each distribution (after make packages)
make ansible-test   # the Ansible collection, linted and run twice: the second run changes nothing (after make packages)
make screenshots    # the documentation's pictures of the web UI, from a real server
```

The Ansible test needs ansible-core 2.20, ansible-lint and the collections `ansible.posix`, `community.general` and
`community.docker`. The lab in [lab/](lab/README.md) runs the slower checks on Ubuntu, AlmaLinux and Oracle Linux
VMs with Ceph.

CI runs the unit tests, the end-to-end suite, the installer's test and the collection's test on every pull request.

`main` is protected (the repository's ruleset `main`, which nobody bypasses): a change lands only through a pull
request whose three CI checks pass on a branch up to date with `main`, merged by rebase or squash, so the history stays
linear. Nothing is pushed to `main` directly, and it is never force-pushed or deleted. Merge one pull request at a
time: the next one is brought up to date and tested again before it can merge.

## A pull request

- **Tests.** Every feature comes with end-to-end tests against real restic and S3, in `test/e2e`.
- **Documentation in the same pull request.** A change of behaviour updates [docs/spec.md](docs/spec.md) and the
  documentation site, [website/index.html](website/index.html), and adds a line under *Unreleased* in
  [CHANGELOG.md](CHANGELOG.md).
- **Decisions are recorded.** A new design decision gets a record, `docs/decisions/NNN-*.md`, with its reasons.
- **Licences.** Holdfast is Apache-2.0. A dependency linked into a program or vendored into the UI must be under MIT,
  BSD, ISC, Apache-2.0 or MPL-2.0; `scripts/third-party-notices.sh` stops the package build otherwise.

By contributing, you agree that your contribution is licensed under the Apache License 2.0, as the rest of Holdfast.

## Making a release

Until 0.1.0, Holdfast is released as **0.1-dev**, a development release (decision 054). Its notes are the
`## 0.1-dev` section of `CHANGELOG.md`, where each change adds its line. `scripts/release.sh 0.1-dev`, on a clean
`main`, builds the packages and the collection at this commit's build, such as `0.1.0~dev90` (`scripts/version.sh`),
deletes the release v0.1-dev and its tag, tags the commit v0.1-dev again, and publishes the new release. Nothing else
changes.

A release with a version of its own:

1. Move the *Unreleased* notes in `CHANGELOG.md` under the new version, and set `version:` in `ansible/galaxy.yml`
   to it.
2. Run `scripts/release.sh <version>` on a clean `main`. It builds the packages and the collection at that version,
   tags the commit, and publishes them as a GitHub release.
3. Set `next=` in `scripts/version.sh` and `version:` in `ansible/galaxy.yml` to the version after it, and update
   the pinned collection URL in the README, `ansible/README.md` and `docs/install.md`.
