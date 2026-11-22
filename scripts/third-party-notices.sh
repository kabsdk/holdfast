#!/bin/bash
# Writes THIRD-PARTY-NOTICES to stdout: the licence, in full, of everything Holdfast's programs and UI contain
# besides Holdfast's own code (decision 043). packaging/build.sh puts it in every package except holdfast-restic,
# which carries restic's own licence.
#
# The Go modules are the ones linked into cmd/... for linux/amd64, with their licence files read from the module
# cache, so run it after `make build`. The rest is in packaging/licenses/. It stops at a module with no licence
# file, and at one under the GPL family, the SSPL or the BUSL: Holdfast is Apache-2.0, and its packages contain
# only what that allows.
#
# Usage: scripts/third-party-notices.sh [holdfast version]
set -euo pipefail
cd "$(dirname "$0")/.."
export GOOS=linux GOARCH=amd64 CGO_ENABLED=0
lic=packaging/licenses
rule=$(printf '=%.0s' $(seq 80))

refuse() {
  if grep -qiE 'GNU (Affero |Lesser )?General Public License|Server Side Public License|Business Source License' "$1"; then
    echo "$1: a licence Holdfast's packages may not contain (decision 043)" >&2
    exit 1
  fi
}

# section <title> <file>...: one piece of software and its licence files.
section() {
  local title=$1
  shift
  printf '\n%s\n%s\n' "$rule" "$title"
  for f in "$@"; do
    refuse "$f"
    printf -- '\n--- %s\n\n' "$(basename "$f")"
    cat "$f"
  done
}

modules=$(go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' ./cmd/... | sort -u)

cat <<EOF
Third-party software in Holdfast${1:+ $1}

Holdfast is under the Apache License, Version 2.0 (LICENSE and NOTICE, beside this file). Its programs are built
with Go and the Go modules below, and its web UI includes htm and Preact. Each is under its own licence, given
here in full.
EOF

section "The Go standard library, $(go env GOVERSION): https://go.dev" "$lic/go.LICENSE" "$lic/go.PATENTS"
while read -r path version dir; do
  mapfile -t files < <(find "$dir" -maxdepth 1 -type f \( -iname 'licen[cs]e*' -o -iname 'copying*' -o -iname 'notice*' -o -iname 'patents*' \) | sort)
  if [ ${#files[@]} -eq 0 ]; then
    echo "$path $version has no licence file: check its licence by hand (decision 043)" >&2
    exit 1
  fi
  section "$path $version" "${files[@]}"
done <<<"$modules"
section "htm 3.1.1, in the web UI: https://github.com/developit/htm" "$lic/htm-3.1.1.LICENSE"
section "Preact 10, in the web UI with htm: https://preactjs.com" "$lic/preact-10.LICENSE"
