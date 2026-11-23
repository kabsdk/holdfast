#!/bin/sh
# Prints the version of a build between releases: <next release>~dev<commits>, which sorts before that
# release in both dpkg and rpm. A tree with changes not committed adds the time, so each such build is
# newer than the one before. A release sets HOLDFAST_VERSION itself (scripts/release.sh 0.7.0). 0.6.0 is released.
next=0.7.0
n=$(git rev-list --count HEAD 2>/dev/null || echo 0)
v="$next~dev$n"
if ! git diff --quiet HEAD -- 2>/dev/null; then
  v="$v.$(date -u +%Y%m%d%H%M%S)"
fi
echo "$v"
