# 048: `host add` adds a host, `job set` adds jobs, with defaults

Status: accepted, 2026-11-23. Adding a host and adding a job should be different commands: a host can exist without a
job, and a `host add` that had to carry the host's first job, and only the first, was awkward.

## Decision

- **`holdfast host add <fqdn>`** takes only the FQDN, and prints the one line to run on the host. The API's
  `POST /api/v1/hosts` takes only `fqdn`.
- **`holdfast job set <fqdn> <job> --path …`** makes or changes a job. A new job takes what it does not give from the
  default job: daily at 02:00 with 20 minutes' spread; one snapshot per day for 14 days, per week for 8 weeks, per
  month for 12 months, per year for 3 years; and always the newest 3. A new job that names a schedule form takes the
  schedule as given, with no spread unless it gives one. An existing job keeps its own values for what is
  not given: a schedule form (`--daily`, `--every`, …) replaces only the form, a spread or zone alone is applied on top,
  retention given is replaced whole, and paths and a dump are each other's alternative.
- **The UI's Add host** asks for the FQDN and the paths of a first job, then makes both, with the same two calls.

## What it costs

Nothing. A host without a job raises `HoldfastFirstBackupMissing` and the problem it has always had.

## Why

One way to make a job instead of two; a retention that passes the checks without typing it; and `host add` means what
it says.

## What changes

`cmd/holdfast` (`host add`, `job set`), `internal/server/jobinput.go`, the UI's Add host page, spec §5.7.
