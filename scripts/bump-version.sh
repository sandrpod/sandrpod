#!/usr/bin/env bash
# Copyright 2026 SandrPod Contributors
#
# Rewrite every pinned version in one pass, so a release cannot leave some of
# them behind. Run scripts/check-versions.sh afterwards (this does) to prove it.
#
#   make bump-version VERSION=v0.6.0
set -euo pipefail
cd "$(dirname "$0")/.."

v="${1:-}"
[ -n "$v" ] || { echo "usage: $0 v<major>.<minor>.<patch>" >&2; exit 1; }
case "$v" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "version must look like v0.6.0, got: $v" >&2; exit 1 ;;
esac
bare="${v#v}"

# sed -i differs between GNU and BSD; pick the right spelling once.
if sed --version >/dev/null 2>&1; then inplace=(-i); else inplace=(-i ''); fi

command -v git >/dev/null || { echo "bump-version needs git (it rewrites tracked files only)" >&2; exit 1; }

files=$(git ls-files '*.md' '*.yml' '*.yaml' 'setup.py' '**/setup.py' | grep -v '^media/')

# shellcheck disable=SC2086
sed "${inplace[@]}" -E \
  -e "s|(ghcr\.io/sandrpod/[a-z-]*:)v[0-9][0-9.]*|\1${v}|g" \
  -e "s|^(> \*\*(Version\|版本)\*\*: )v[0-9][0-9.]*|\1${v}|" \
  -e "s|^(    version=\")[0-9][0-9.]*(\",)|\1${bare}\2|" \
  $files

# Scope the report to the files this script touched, not everything dirty.
echo "bumped to $v:"
# shellcheck disable=SC2086
git diff --name-only -- $files | sed 's/^/  /'
echo
exec ./scripts/check-versions.sh
