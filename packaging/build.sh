#!/usr/bin/env bash
# Builds the .deb and .rpm packages into dist/, with SHA256SUMS (unsigned for now).
# `make packages` runs it after building the binaries; nfpm runs in a container, so the host needs no
# packaging tools.
#
#   HOLDFAST_VERSION     the packages' version, such as 0.1.0 (required)
#   RESTIC_BINARY        the pinned restic, checked (scripts/fetch-restic.sh)
#   RESTIC_VERSION       its version
#   NFPM_IMAGE           nfpm's image, pinned by digest
#   HOLDFAST_MAINTAINER  the packages' maintainer field
set -euo pipefail
cd "$(dirname "$0")/.."
: "${HOLDFAST_VERSION:?}" "${RESTIC_BINARY:?}" "${RESTIC_VERSION:?}" "${NFPM_IMAGE:?}"
export HOLDFAST_MAINTAINER="${HOLDFAST_MAINTAINER:-Holdfast maintainers}"

for b in holdfast-agent holdfast-server holdfast; do
  [ -x "bin/$b" ] || { echo "bin/$b is missing: make build first" >&2; exit 1; }
done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p dist "$work/scripts"
cp "$RESTIC_BINARY" "$work/restic"

# The licences of what the programs contain (decision 043), beside Holdfast's LICENSE and NOTICE in each package.
scripts/third-party-notices.sh "$HOLDFAST_VERSION" >"$work/THIRD-PARTY-NOTICES"

# Each package's scripts name its own unit.
declare -A unit=([holdfast-agent]=holdfast-agent.service [holdfast-server]=holdfast-server.service)
for pkg in "${!unit[@]}"; do
  for s in postinstall preremove; do
    sed -e "s/@PACKAGE@/$pkg/g" -e "s/@UNIT@/${unit[$pkg]}/g" "packaging/$s.sh.in" >"$work/scripts/$s-$pkg.sh"
  done
done

# The configs' ${...} are filled in here: nfpm expands variables in some fields only.
for pkg in holdfast-restic holdfast-agent holdfast-server holdfast-cli; do
  cfg="packaging/nfpm-${pkg#holdfast-}.yaml"
  sed -e "s#packaging/scripts/postinstall.sh#$work/scripts/postinstall-$pkg.sh#" \
      -e "s#packaging/scripts/preremove.sh#$work/scripts/preremove-$pkg.sh#" \
      -e "s#\${HOLDFAST_VERSION}#$HOLDFAST_VERSION#g" -e "s#\${HOLDFAST_MAINTAINER}#$HOLDFAST_MAINTAINER#g" \
      -e "s#\${RESTIC_VERSION}#$RESTIC_VERSION#g" -e "s#\${RESTIC_BINARY}#$work/restic#g" \
      -e "s#\${THIRD_PARTY_NOTICES}#$work/THIRD-PARTY-NOTICES#g" "$cfg" >"$work/$pkg.yaml"
  for fmt in deb rpm; do
    docker run --rm -u "$(id -u):$(id -g)" -v "$PWD:$PWD" -v "$work:$work" -w "$PWD" \
      "$NFPM_IMAGE" package --config "$work/$pkg.yaml" --packager "$fmt" --target dist/ >/dev/null
  done
done

# A release is what dist/ holds: the packages, install.sh (decision 044), SHA256SUMS of this build's packages and
# install.sh, and VERSION, which install.sh reads to name the packages. A file's name spells the version's "~" as ".",
# as GitHub renames a release's files: a build between releases, such as 0.1.0~dev90, is in holdfast-agent_0.1.0.dev90-1_amd64.deb.
# The package inside keeps its "~", so it sorts before 0.1.0.
install -m 0755 scripts/install.sh dist/install.sh
for f in dist/*~*; do [ -e "$f" ] && mv "$f" "${f//\~/.}"; done
fv=${HOLDFAST_VERSION//\~/.}
( cd dist && sha256sum ./*_"${fv}"-1_amd64.deb ./*-"${fv}"-1.x86_64.rpm ./install.sh | sed 's# \./# #' >SHA256SUMS )
echo "$HOLDFAST_VERSION-1" >dist/VERSION
cat dist/SHA256SUMS
