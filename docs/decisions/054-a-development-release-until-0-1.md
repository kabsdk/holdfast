# 054: A development release, 0.1-dev, until 0.1.0

Status: accepted, 2026-11-24.

## Decision

- **Until 0.1.0, Holdfast is released as 0.1-dev**, a development release, subject to change. Each one replaces the
  one before: `scripts/release.sh 0.1-dev` deletes the release and moves the tag `v0.1-dev` to the new commit.
- **Its packages are the build between releases** that `scripts/version.sh` names, such as `0.1.0~dev90`: they sort
  before 0.1.0 in dpkg and rpm, and each newer build sorts after the one before, so installing a newer 0.1-dev, or
  0.1.0 later, upgrades.
- **The release is named for the people who pin it**: `HOLDFAST_VERSION=0.1-dev` in `install.sh`, `holdfast_version:
  0.1-dev` in the Ansible roles, and the line `holdfast host add` prints, which a server built between releases pins
  to the development release of its next version. Each accepts a release `X.Y-dev` that holds a build `X.Y.Z~devN`.
- **A file's name spells the version's `~` as `.`**, as GitHub renames a release's files: the build `0.1.0~dev90` is
  in `holdfast-agent_0.1.0.dev90-1_amd64.deb`. The package inside keeps its `~`. A mirror made from `dist/` has the
  same names.
- The Ansible collection of a development release is also published as `kabsdk-holdfast-0.1-dev.tar.gz`, a name that
  stays as the release moves on.

## Why

- Nobody runs Holdfast yet, and the first release is still settling. A version that moves until it is ready says that
  honestly, and saves numbering releases that are not meant to last.
- A version that sorts before the release it leads to makes upgrades plain package upgrades, with no special case.
- GitHub turns `~` into `.` in a release's file names, and a git tag may not hold `~` at all, so the names that are
  fetched cannot be the package versions themselves.

## What it costs

- A pinned `0.1-dev` is not one build: installing it again later may install a newer one, and an agent may be newer
  than its server, which it says ("upgrade the server first").
