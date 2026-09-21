#!/bin/bash
# Build and publish an Incus image.
#
# Usage: ./scripts/build-image.sh <image-name> [ref]
#   image-name: base | edge | manager | restaurant | gitea | plane | bookstack
#   ref:        git tag/branch for app images
#
# For base/edge/gitea/plane/bookstack: builds from the image recipe in images/
# For manager/restaurant: builds FROM opsavor-base; images/<name>/build.sh
# clones the app repo at <ref> itself (via $MANAGER_REPO / $RESTAURANT_REPO).
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
  edge|manager|restaurant)
    FROM="opsavor-base"
    ;;
  *)
    FROM="opsavor-base"
    ;;
esac

echo "[build] Building $ALIAS from $FROM"
incus delete "$TMP_CT" --force 2>/dev/null || true
incus launch "$FROM" "$TMP_CT" --profile base

echo "  waiting for network..."
for i in $(seq 1 30); do
  if incus exec "$TMP_CT" -- ping -c 1 -W 2 8.8.8.8 >/dev/null 2>&1; then
    break
  fi
  sleep 2
done

# Push the build recipe
incus file push -r "$BUILD_DIR/" "$TMP_CT/tmp/build/"

# manager/restaurant clone their own app source over SSH inside build.sh
# (git.theta42.com via the gitea_deploy key), but that key and its
# accept-new host config live on the HOST's /root/.ssh, not inside this
# fresh temp container — without pushing them in first, the clone fails
# outright with "Host key verification failed". The container is destroyed
# right after this build, so the exposure is no wider than the host's own
# already-standing trust in that key.
if [ "$IMAGE_NAME" = "manager" ] || [ "$IMAGE_NAME" = "restaurant" ]; then
  incus exec "$TMP_CT" -- mkdir -p /root/.ssh
  incus file push /root/.ssh/gitea_deploy "$TMP_CT/root/.ssh/gitea_deploy"
  [ -f /root/.ssh/config ] && incus file push /root/.ssh/config "$TMP_CT/root/.ssh/config"
  incus exec "$TMP_CT" -- chmod 700 /root/.ssh
  incus exec "$TMP_CT" -- chmod 600 /root/.ssh/gitea_deploy /root/.ssh/config
fi

# Run build (manager/restaurant clone their own app source inside build.sh;
# base/edge/gitea/plane/bookstack ignore $2 entirely)
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
  incus image alias create --reuse "opsavor-${IMAGE_NAME}:latest" "$(image_fingerprint "$ALIAS")" 2>/dev/null || true
fi

incus delete "$TMP_CT" --force
echo "[build] Done: $ALIAS ($(image_fingerprint "$ALIAS"))"
