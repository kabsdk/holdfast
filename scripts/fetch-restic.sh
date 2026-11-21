#!/bin/bash
# Download the pinned restic release for the e2e tests and check it against the release's SHA256SUMS.
# Usage: scripts/fetch-restic.sh [version]   Prints the path of the binary.
set -euo pipefail
version="${1:-0.19.1}"
dir="${HOLDFAST_CACHE:-$HOME/.cache/holdfast}"
bin="$dir/restic-$version"
if [ -x "$bin" ]; then echo "$bin"; exit 0; fi
mkdir -p "$dir"
base="https://github.com/restic/restic/releases/download/v$version"
asset="restic_${version}_linux_amd64.bz2"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "$tmp/$asset" "$base/$asset"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"
( cd "$tmp" && grep " $asset\$" SHA256SUMS | sha256sum -c --quiet - ) || { echo "checksum mismatch for $asset" >&2; exit 1; }
bunzip2 -c "$tmp/$asset" > "$bin.tmp"
chmod 0755 "$bin.tmp"
mv "$bin.tmp" "$bin"
echo "$bin"
