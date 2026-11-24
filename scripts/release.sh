#!/usr/bin/env bash
# A release: the packages, install.sh, VERSION, SHA256SUMS and the Ansible collection, built at one version from a clean
# checkout, the commit tagged v<version>, and all of it published as a GitHub release (gh, logged in).
#   scripts/release.sh 0.1.0 [--draft]
# Run the tests first: make test e2e install-test ansible-test.
set -euo pipefail
cd "$(dirname "$0")/.."
v=${1:?usage: scripts/release.sh <version> [--draft]}
shift
[[ $v =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "$v is not a version such as 1.0.0" >&2; exit 1; }
git diff --quiet HEAD -- || { echo "the tree has changes not committed: commit them first" >&2; exit 1; }
! git rev-parse -q --verify "refs/tags/v$v" >/dev/null || { echo "v$v is tagged already" >&2; exit 1; }
[ "$(sed -n 's/^version: //p' ansible/galaxy.yml)" = "$v" ] || { echo "ansible/galaxy.yml's version is not $v" >&2; exit 1; }
# The release's notes are its section of CHANGELOG.md, from "## <version>" to the next "## ".
changes=$(awk -v h="## $v " 'index($0, h) == 1 {p = 1; next} p && /^## / {exit} p' CHANGELOG.md)
[ -n "$(echo "$changes" | tr -d '[:space:]')" ] || { echo "CHANGELOG.md has no section \"## $v (date)\"" >&2; exit 1; }

rm -rf dist
make packages HOLDFAST_VERSION="$v"
make collection HOLDFAST_VERSION="$v"
files=(dist/install.sh dist/VERSION dist/SHA256SUMS dist/*_"$v"-1_amd64.deb dist/*-"$v"-1.x86_64.rpm "dist/kabsdk-holdfast-$v.tar.gz")
for f in "${files[@]}"; do [ -s "$f" ] || { echo "$f is missing" >&2; exit 1; }; done

git tag -a "v$v" -m "Holdfast $v"
git push origin "v$v"
notes=$(mktemp)
cat >"$notes" <<EOF
Holdfast $v: restic backups for a fleet of Linux servers, from one server, to your own S3.
$changes

**Install the server**, with its settings in the environment (\`sh install.sh help\` lists them):

    curl -sfL https://github.com/kabsdk/holdfast/releases/download/v$v/install.sh | HOLDFAST_VERSION=$v sh -s - server

**Or with Ansible**: the collection kabsdk.holdfast, with the same settings in lower case (ansible/README.md):

    ansible-galaxy collection install https://github.com/kabsdk/holdfast/releases/download/v$v/kabsdk-holdfast-$v.tar.gz

The packages are for x86_64, .deb and .rpm, and are not signed yet: install.sh and the collection check each against
SHA256SUMS. The documentation is at https://kabsdk.github.io/holdfast/.
EOF
gh release create "v$v" "${files[@]}" --title "Holdfast $v" --notes-file "$notes" "$@"
rm -f "$notes"
