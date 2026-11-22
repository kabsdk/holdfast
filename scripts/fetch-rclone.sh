#!/bin/bash
# Download the pinned rclone release for the vault (decision 039) and check it against the release's SHA256SUMS.
# Usage: scripts/fetch-rclone.sh [version]   Prints the path of the binary.
set -euo pipefail
version="${1:-1.76.0}"
dir="${HOLDFAST_CACHE:-$HOME/.cache/holdfast}"
bin="$dir/rclone-$version"
if [ -x "$bin" ]; then echo "$bin"; exit 0; fi
mkdir -p "$dir"
base="https://downloads.rclone.org/v$version"
asset="rclone-v${version}-linux-amd64.zip"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "$tmp/$asset" "$base/$asset"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"
( cd "$tmp" && grep " $asset\$" SHA256SUMS | sha256sum -c --quiet - ) || { echo "checksum mismatch for $asset" >&2; exit 1; }
python3 -I -c 'import sys, zipfile
z = zipfile.ZipFile(sys.argv[1])
name = next(n for n in z.namelist() if n.endswith("/rclone"))
open(sys.argv[2], "wb").write(z.read(name))' "$tmp/$asset" "$bin.tmp"
chmod 0755 "$bin.tmp"
mv "$bin.tmp" "$bin"
echo "$bin"
