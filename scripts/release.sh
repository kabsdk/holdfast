#!/usr/bin/env bash
# A release: the packages, install.sh, VERSION, SHA256SUMS and the Ansible collection, built at one version from a clean
# checkout, the commit tagged v<version>, and all of it published as a GitHub release (gh, logged in).
#   scripts/release.sh 0.1.0 [--draft]
#   scripts/release.sh 0.1-dev    a development release (decision 054): this commit's build between releases, such as
#                                 0.1.0~dev90 (scripts/version.sh), published as v0.1-dev. It replaces the release and
#                                 moves the tag: until 0.1.0 is released, v0.1-dev is the newest build, subject to change.
# Run the tests first: make test e2e install-test ansible-test.
set -euo pipefail
cd "$(dirname "$0")/.."
v=${1:?usage: scripts/release.sh <version> [--draft], or a development release such as 0.1-dev}
shift
git diff --quiet HEAD -- || { echo "the tree has changes not committed: commit them first" >&2; exit 1; }
if [[ $v =~ ^([0-9]+)\.([0-9]+)-dev$ ]]; then
  dev=1
  pkg=$(sh scripts/version.sh)
  [[ $pkg == "${BASH_REMATCH[1]}.${BASH_REMATCH[2]}."*"~dev"* ]] ||
    { echo "scripts/version.sh gives $pkg, not a build of $v: set its next= to ${v%-dev}.0" >&2; exit 1; }
else
  dev=
  [[ $v =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "$v is not a version such as 1.0.0, or a development release such as 0.1-dev" >&2; exit 1; }
  ! git rev-parse -q --verify "refs/tags/v$v" >/dev/null || { echo "v$v is tagged already" >&2; exit 1; }
  [ "$(sed -n 's/^version: //p' ansible/galaxy.yml)" = "$v" ] || { echo "ansible/galaxy.yml's version is not $v" >&2; exit 1; }
  pkg=$v
fi
# The release's notes are its section of CHANGELOG.md, from "## <version>", alone or followed by a space, to the next "## ".
changes=$(awk -v h="## $v" '$0 == h || index($0, h " ") == 1 {p = 1; next} p && /^## / {exit} p' CHANGELOG.md)
[ -n "$(echo "$changes" | tr -d '[:space:]')" ] || { echo "CHANGELOG.md has no section \"## $v\"" >&2; exit 1; }

rm -rf dist
make packages HOLDFAST_VERSION="$pkg"
make collection HOLDFAST_VERSION="$pkg"
fv=${pkg//\~/.} # a file's name spells the version's "~" as "." (packaging/build.sh)
collection="dist/kabsdk-holdfast-${pkg/\~/-}.tar.gz"
if [ -n "$dev" ]; then
  # A name that stays the same as the release moves on, so the documentation can point at it.
  cp "$collection" "dist/kabsdk-holdfast-$v.tar.gz"
  collection="dist/kabsdk-holdfast-$v.tar.gz"
fi
files=(dist/install.sh dist/VERSION dist/SHA256SUMS dist/*_"$fv"-1_amd64.deb dist/*-"$fv"-1.x86_64.rpm "$collection")
for f in "${files[@]}"; do [ -s "$f" ] || { echo "$f is missing" >&2; exit 1; }; done

if [ -n "$dev" ]; then
  # The development release moves on: the one before, and its tag, go.
  if gh release view "v$v" >/dev/null 2>&1; then gh release delete "v$v" --cleanup-tag --yes; fi
  git tag -d "v$v" >/dev/null 2>&1 || true
  git push -q origin ":refs/tags/v$v" 2>/dev/null || true
fi
git tag -a "v$v" -m "Holdfast $v"
git push origin "v$v"
notes=$(mktemp)
{
  if [ -n "$dev" ]; then
    echo "**A development release, subject to change**: the build $pkg, of commit $(git rev-parse --short HEAD). Until"
    echo "${v%-dev}.0 is released, newer builds replace this release, under the same tag, v$v."
    echo
  fi
  cat <<EOF
Holdfast $v: restic backups for a fleet of Linux servers, from one server, to your own S3.
$changes

**Install the server**, with its settings in the environment (\`sh install.sh help\` lists them):

    curl -sfL https://github.com/kabsdk/holdfast/releases/download/v$v/install.sh | HOLDFAST_VERSION=$v sh -s - server

**Or with Ansible**: the collection kabsdk.holdfast, with the same settings in lower case (ansible/README.md):

    ansible-galaxy collection install https://github.com/kabsdk/holdfast/releases/download/v$v/$(basename "$collection")

The packages are for x86_64, .deb and .rpm, and are not signed yet: install.sh and the collection check each against
SHA256SUMS. The documentation is at https://kabsdk.github.io/holdfast/.
EOF
} >"$notes"
gh release create "v$v" "${files[@]}" --title "Holdfast $v" --notes-file "$notes" "$@"
rm -f "$notes"
