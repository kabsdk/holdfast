# 048: `host add` adds a host, `job set` adds jobs, with defaults

Status: accepted, 2026-11-23 (plan-m10 item 4). You said: "HOST ADD AND job add should be different commands. You can
add a host without a job and making it a requirement that 'hey, you need to run host add but ONLY THE FIRST TIME' is
weird."

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
