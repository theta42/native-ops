#!/bin/bash
# Build and publish an Incus image.
#
# Usage: ./scripts/build-image.sh <image-name> [ref]
#   image-name: base | edge | manager | restaurant | home | gitea | plane
#   ref:        git tag/branch for app images
#
# For base/edge/gitea/plane: builds from the image recipe in images/
# For manager/restaurant/home: builds FROM opsavor-base; images/<name>/build.sh
# clones the app repo at <ref> itself (via $MANAGER_REPO / $RESTAURANT_REPO /
# $HOME_REPO).
#
# The image is built on the Incus host. Temp container -> publish -> delete.
set -euo pipefail

IMAGE_NAME="${1:?usage: build-image.sh <name> [ref]}"
REF="${2:-}"
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_DIR="$REPO_DIR/images/$IMAGE_NAME"
source "$REPO_DIR/scripts/lib.sh"

[ -d "$BUILD_DIR" ] || { echo "no image recipe at $BUILD_DIR" >&2; exit 1; }

TMP_CT="build-${IMAGE_NAME}-$$"
ALIAS="opsavor-${IMAGE_NAME}"
[ -n "$REF" ] && ALIAS="${ALIAS}:${REF}"

# Base image selection
case "$IMAGE_NAME" in
  base)
    FROM="images:debian/13"
    ;;
  edge|manager|restaurant|home)
    FROM="opsavor-base"
    ;;
  *)
    FROM="opsavor-base"
    ;;
esac

# The `base` profile caps a container at 512MB, which is enough to run an
# app but not to build one: `npm ci` on the restaurant app (Next.js +
# drizzle-kit + typescript + eslint as devDependencies, plus better-sqlite3
# possibly compiling from source) gets OOM-killed well short of finishing —
# and even at 3GB, `next build`'s TypeScript-checking phase (Next 16 +
# Turbopack) still got OOM-killed. Give build containers real headroom
# regardless of what the published image will run with; the host has it
# (8GB, mostly idle during a build).
BUILD_LIMITS=(--config limits.cpu=2 --config limits.memory=5GB)

echo "[build] Building $ALIAS from $FROM"
incus delete "$TMP_CT" --force 2>/dev/null || true
incus launch "$FROM" "$TMP_CT" --profile base "${BUILD_LIMITS[@]}"

echo "  waiting for network..."
for i in $(seq 1 30); do
  if incus exec "$TMP_CT" -- ping -c 1 -W 2 8.8.8.8 >/dev/null 2>&1; then
    break
  fi
  sleep 2
done

# Push the build recipe
incus file push -r "$BUILD_DIR/" "$TMP_CT/tmp/build/"

# manager/restaurant/home clone their own app source over SSH inside build.sh,
# but the deploy key and its accept-new host config live on the HOST's
# /root/.ssh, not inside this fresh temp container — without pushing them in
# first, the clone fails outright with "Host key verification failed". The
# container is destroyed right after this build, so the exposure is no wider
# than the host's own already-standing trust in that key.
#
# manager/restaurant pull from git.theta42.com (gitea_deploy); home pulls from
# the in-fleet Gitea at git.opsavor.work, which needs its own deploy key
# (gitea_opsavor_deploy) — image/management as explicit, per-host keys.
case "$IMAGE_NAME" in
  manager|restaurant) DEPLOY_KEY_SRC=/root/.ssh/gitea_deploy; DEPLOY_KEY_NAME=gitea_deploy ;;
  home)               DEPLOY_KEY_SRC=/root/.ssh/gitea_opsavor_deploy; DEPLOY_KEY_NAME=gitea_opsavor_deploy ;;
  *)                  DEPLOY_KEY_SRC="" ;;
esac

if [ -n "$DEPLOY_KEY_SRC" ]; then
  if [ ! -f "$DEPLOY_KEY_SRC" ]; then
    echo "[build] $DEPLOY_KEY_SRC missing — cannot clone the app repo inside the build container" >&2
    echo "        (create a read-only deploy key for the repo and place it there)" >&2
    exit 1
  fi
  incus exec "$TMP_CT" -- mkdir -p /root/.ssh
  incus file push "$DEPLOY_KEY_SRC" "$TMP_CT/root/.ssh/$DEPLOY_KEY_NAME"
  [ -f /root/.ssh/config ] && incus file push /root/.ssh/config "$TMP_CT/root/.ssh/config"
  incus exec "$TMP_CT" -- chmod 700 /root/.ssh
  incus exec "$TMP_CT" -- chmod 600 "/root/.ssh/$DEPLOY_KEY_NAME"
  [ -f /root/.ssh/config ] && incus exec "$TMP_CT" -- chmod 600 /root/.ssh/config
fi

# Run build (manager/restaurant/home clone their own app source inside
# build.sh; base/edge/gitea/plane ignore $2 entirely)
if [ -n "$REF" ]; then
  incus exec "$TMP_CT" -- bash "/tmp/build/build.sh" "$REF"
else
  incus exec "$TMP_CT" -- bash "/tmp/build/build.sh"
fi

# Publish
echo "  publishing..."
incus stop "$TMP_CT"
incus publish "$TMP_CT" --alias "$ALIAS" --reuse
if [ -n "$REF" ]; then
  # Two gotchas here, both previously masked by `|| true` swallowing the
  # real error:
  # 1. `incus image alias create` has no --reuse flag (only `incus publish`
  #    does), so :latest was never actually repointed.
  # 2. Its <new alias name> argument is itself parsed as [<remote>:]<name>
  #    on the FIRST colon — "opsavor-restaurant:latest" was read as remote
  #    "opsavor-restaurant", resource "latest", and failed with "the remote
  #    ... doesn't exist". Prefixing the explicit `local:` remote makes the
  #    colon split land where intended, leaving the alias itself intact.
  LATEST="opsavor-${IMAGE_NAME}:latest"
  incus image alias delete "local:$LATEST" 2>/dev/null || true
  incus image alias create "local:$LATEST" "$(image_fingerprint "$ALIAS")"
fi

incus delete "$TMP_CT" --force
echo "[build] Done: $ALIAS ($(image_fingerprint "$ALIAS"))"
