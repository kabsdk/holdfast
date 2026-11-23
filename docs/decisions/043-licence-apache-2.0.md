# 043: Holdfast's licence is Apache-2.0

Status: accepted, 2026-11-22. The licence had been open since M1.

## Decision

1. **Holdfast is under the Apache License, Version 2.0**, all of it: the programs, the UI, the docs and the
   lab. `LICENSE` holds the text. `NOTICE` says "Copyright 2026 The Holdfast authors".
2. **There is no contributor licence agreement.** A contribution comes in under the same licence (Apache-2.0 §5).
   Each author keeps the copyright in what they wrote, and "The Holdfast authors" names them together.
3. **The packages carry the licences of what they contain.** Each one has `/usr/share/doc/<package>/` with Holdfast's
   `LICENSE` and `NOTICE` and a `THIRD-PARTY-NOTICES` file. That file gives, in full, the licences of the Go standard
   library, of every Go module linked into the programs, and of htm and Preact, which the UI includes. `holdfast-restic`
   carries restic's own licence, BSD-2-Clause. The rpm's licence field says Apache-2.0, or BSD-2-Clause for
   `holdfast-restic`.
4. **What Holdfast may contain.** Anything linked into a program or vendored into the UI must be under a licence
   that allows shipping it inside an Apache-2.0 product: MIT, BSD, ISC, Apache-2.0, MPL-2.0 and the like. The GPL
   family, the SSPL and the BUSL are not allowed. `scripts/third-party-notices.sh` stops the package build if one of
   them appears, or if a module has no licence file. What only runs the tests is not shipped, so these rules do not
   apply to it: MinIO's server, for example, is AGPL-3.0.

## What it costs

- Anyone may take Holdfast, change it, and sell it or run it for others without publishing their changes. A copyleft
  licence such as the GPL-3.0 or AGPL-3.0 would oblige them to; Apache-2.0 does not.
- Holdfast's code cannot go into a GPL-2.0-only project, though it can go into a GPL-3.0 one.
- The notices must be kept current. When Go, restic or the UI's vendored module is updated, its licence file in
  `packaging/licenses/` is fetched again. The Go modules' licences are read from the module cache at every package
  build, so they cannot go stale.

## Why

- **The audience is ops teams at companies (decision 042).** Their legal review passes Apache-2.0, MIT and BSD as a
  matter of routine. Many companies forbid the AGPL outright for anything they run, and some forbid the GPL. A licence
  that keeps Holdfast off a company's approved list fails the people it is for.
- **There is little for copyleft to protect.** The AGPL's main gain is that whoever runs a changed copy as a service
  must publish their changes. Holdfast is one server, run by the team that owns the bucket (decision 042). It is not a
  service to resell, and it has no business model that a fork could undercut.
- **It has patent terms; MIT and BSD do not.** Apache-2.0 gives every user a patent licence from every contributor,
  and ends that licence for anyone who sues over a patent in the work. MIT and BSD say nothing about patents. For a
  free project that takes outside contributions, that is why Apache-2.0 is chosen over MIT.
- **It matches what Holdfast is built from.** restic is BSD-2-Clause. Kopia, minio-go, cobra and go-oidc are
  Apache-2.0. Every module Holdfast links is under MIT, BSD or Apache-2.0, and an Apache-2.0 product can contain all
  of them as they are. Backrest, a web UI for restic on each host, chose GPL-3.0. That suits a tool each person runs
  for themselves, but for one a company installs centrally the reasons above point the other way.

## What changes

- `LICENSE` and `NOTICE` at the root of the repository, and a Licence section in the README.
- The packages: the licence field, `/usr/share/doc/<package>/`, and `scripts/third-party-notices.sh`, which
  `packaging/build.sh` runs. The third-party licence files the module cache does not hold are in `packaging/licenses/`.
- CLAUDE.md's Repository section says what Holdfast may contain.
- The packages' maintainer field is still open.
