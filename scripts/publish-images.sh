#!/usr/bin/env bash
# Publish SandrPod container images to GHCR from a local machine.
#
# The repository is not hosted on GitHub, so the GitHub Actions release
# workflow does not run for it. This script does the equivalent locally:
# log in to GHCR with a GitHub PAT and push multi-arch images.
#
# Usage:
#   GITHUB_PAT=ghp_xxx GHCR_USER=<your-github-username> \
#     ./scripts/publish-images.sh v0.3.1
#
# Env:
#   GITHUB_PAT   GitHub personal access token with write:packages (for login).
#                If unset, the script assumes you already ran `docker login ghcr.io`.
#   GHCR_USER    GitHub username used for `docker login` (default: $OWNER).
#   OWNER        GHCR namespace / GitHub org (default: sandrpod).
#   PLATFORMS    Multi-arch list for poder/toolbox (default: linux/amd64,linux/arm64).
#   PUSH_CENTOS  Set to 0 to skip the (amd64-only) CentOS toolbox image.
set -euo pipefail

TAG="${1:-latest}"
OWNER="${OWNER:-sandrpod}"
REGISTRY="ghcr.io"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
PUSH_CENTOS="${PUSH_CENTOS:-1}"

cd "$(dirname "$0")/.."

if [ -n "${GITHUB_PAT:-}" ]; then
  echo "→ logging in to ${REGISTRY} as ${GHCR_USER:-$OWNER}"
  echo "$GITHUB_PAT" | docker login "$REGISTRY" -u "${GHCR_USER:-$OWNER}" --password-stdin
else
  echo "→ GITHUB_PAT not set; assuming you already ran 'docker login ${REGISTRY}'"
fi

# name  dockerfile  platforms
publish() {
  local name="$1" dockerfile="$2" platforms="$3"
  local img="${REGISTRY}/${OWNER}/${name}"
  echo "→ building & pushing ${img}:{${TAG},latest} (${platforms})"
  docker buildx build \
    --platform "$platforms" \
    -f "$dockerfile" \
    -t "${img}:${TAG}" \
    -t "${img}:latest" \
    --push .
}

publish poder   docker/Dockerfile.poder   "$PLATFORMS"
publish toolbox docker/Dockerfile.toolbox "$PLATFORMS"

if [ "$PUSH_CENTOS" = "1" ]; then
  # CentOS image is amd64-only: arm64 under QEMU is slow.
  publish toolbox-centos docker/Dockerfile.toolbox.centos "linux/amd64"
fi

echo "✓ published to ${REGISTRY}/${OWNER}: poder, toolbox$([ "$PUSH_CENTOS" = "1" ] && echo ", toolbox-centos") @ ${TAG}"
echo "  Remember: make the packages public (or configure pull auth) so VMs can pull them."
