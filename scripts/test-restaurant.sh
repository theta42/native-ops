#!/bin/bash
# Prove green: clone the restaurant repo at <ref> into a disposable Incus
# container and run its full test suite. `npm test` there runs `next build`
# first (typecheck + build), then the ~1000-test node:test suite — so this
# one container run exercises both.
#
# Used by the restaurant release workflow, BEFORE build-image.sh: the
# release must not publish an image or roll the fleet from a tag that
# doesn't pass its own suite.
#
# Usage: ./scripts/test-restaurant.sh <ref>
set -euo pipefail

REF="${1:?usage: test-restaurant.sh <ref>}"
REPO_URL="${RESTAURANT_REPO:-ssh://gitea@git.theta42.com:2222/opsavor/restaurant.git}"
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
source "$REPO_DIR/scripts/lib.sh"

TMP_CT="test-restaurant-$$"
# Same headroom as the production build (images/restaurant/build.sh: even
# 3GB gets OOM-killed by `next build`'s TypeScript-checking phase).
TEST_LIMITS=(--config limits.cpu=2 --config limits.memory=5GB)

cleanup() { incus delete "$TMP_CT" --force 2>/dev/null || true; }
trap cleanup EXIT

echo "[test] Launching $TMP_CT to test $REF"
incus delete "$TMP_CT" --force 2>/dev/null || true
incus launch opsavor-base "$TMP_CT" --profile base "${TEST_LIMITS[@]}"

echo "  waiting for network..."
for i in $(seq 1 30); do
  if incus exec "$TMP_CT" -- ping -c 1 -W 2 8.8.8.8 >/dev/null 2>&1; then
    break
  fi
  sleep 2
done

incus exec "$TMP_CT" -- bash -c \
  "apt-get update -qq && apt-get install -y -qq --no-install-recommends ca-certificates curl git sqlite3 build-essential python3"

# gitea_deploy key + host config, same as build-image.sh's manager/restaurant
# clone step — see that script's comment for why these are pushed fresh into
# the temp container rather than baked into the image.
incus exec "$TMP_CT" -- mkdir -p /root/.ssh
incus file push /root/.ssh/gitea_deploy "$TMP_CT/root/.ssh/gitea_deploy"
[ -f /root/.ssh/config ] && incus file push /root/.ssh/config "$TMP_CT/root/.ssh/config"
incus exec "$TMP_CT" -- chmod 700 /root/.ssh
incus exec "$TMP_CT" -- chmod 600 /root/.ssh/gitea_deploy

incus exec "$TMP_CT" -- git clone --depth 1 --branch "$REF" "$REPO_URL" /tmp/restaurant-src

incus exec "$TMP_CT" -- bash -c \
  "cd /tmp/restaurant-src && NODE_OPTIONS=--max-old-space-size=2560 npm ci --no-audit --no-fund && npm test"

echo "[test] $REF is green"
