#!/usr/bin/env bash
# Copyright 2026 SandrPod Contributors
#
# Every place the repo pins a released version must agree with every other.
#
# This deliberately does NOT know what the current version is — it only checks
# internal consistency, so it passes on any branch and on the release bump
# itself, as long as the bump covered everything. That is the failure it exists
# to catch: the eight provider guides sat at v0.5.0 for four releases because
# each bump touched the compose file and the docs someone happened to remember.
#
#   scripts/check-versions.sh          # verify
#   make bump-version VERSION=v0.6.0   # rewrite them all
# media/ is excluded: the published article pins the version its walkthrough
# was actually run against, and rewriting that would make it a lie.
set -euo pipefail
cd "$(dirname "$0")/.."

fail=0
tmp=$(mktemp); trap 'rm -f "$tmp"' EXIT

# Pinned container images: ghcr.io/sandrpod/<name>:vX.Y.Z
grep -rn --include='*.md' --include='*.yml' --include='*.yaml' \
     -o 'ghcr\.io/sandrpod/[a-z-]*:v[0-9][0-9.]*' . 2>/dev/null \
  | grep -Ev '^\./(\.git|media)/' \
  | sed 's/.*:v\([0-9][0-9.]*\)$/\1|&/' >> "$tmp" || true

# Doc version headers, both languages.
grep -rn --include='*.md' -E '^> \*\*(Version|版本)\*\*: v[0-9]' . 2>/dev/null \
  | grep -Ev '^\./(\.git|media)/' \
  | sed 's/.*: v\([0-9][0-9.]*\).*/\1|&/' >> "$tmp" || true

# The Python SDK, which pins without the leading v.
grep -rn --include='setup.py' -E 'version="[0-9]' . 2>/dev/null \
  | grep -Ev '^\./(\.git|media)/' \
  | sed 's/.*version="\([0-9][0-9.]*\)".*/\1|&/' >> "$tmp" || true

if [ ! -s "$tmp" ]; then
  echo "check-versions: found nothing to check — the patterns have drifted from the repo" >&2
  exit 1
fi

versions=$(cut -d'|' -f1 "$tmp" | sort -u)
count=$(printf '%s\n' "$versions" | grep -c .)

if [ "$count" -ne 1 ]; then
  echo "check-versions: pinned versions disagree."
  echo
  # Show the minority first — that is almost always what was forgotten.
  for v in $versions; do
    n=$(cut -d'|' -f1 "$tmp" | grep -cx "$v")
    echo "  $v — $n reference(s)"
  done
  echo
  echo "Where:"
  sort -t'|' -k1,1 "$tmp" | while IFS='|' read -r v rest; do
    printf '  %-10s %s\n' "$v" "${rest%%:ghcr*}"
  done | sed 's/  *$//'
  echo
  echo "Fix with:  make bump-version VERSION=v<the one you meant>"
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  echo "check-versions: all pinned versions agree ($versions)"
fi
exit "$fail"
