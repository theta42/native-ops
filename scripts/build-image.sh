#!/bin/bash
# Build and publish an Incus image.
#
# Usage: ./scripts/build-image.sh <image-name> [ref]
#   image-name: base | edge | manager | restaurant | gitea | plane | bookstack
#   ref:        git tag/branch for app images
#
# For base/edge/gitea/plane/bookstack: builds from the image recipe in images/
# For manager/restaurant: builds FROM opsavor-base, clones the app repo at <ref>
#
# The image is built on the Incus host. Temp container → publish → delete.
set -euo pipefail

IMAGE_NAME="${1:?usage: build-image.sh <name> [ref]}"
REF="${2:-}"
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_DIR="$REPO_DIR/images/$IMAGE_NAME"

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

# For manager/restaurant (tag-based builds), also clone the app source
if [ "$IMAGE_NAME" = "manager" ] || [ "$IMAGE_NAME" = "restaurant" ]; then
  APP_REPO="${APP_REPO:-/root/${IMAGE_NAME}}"
  if [ -n "$REF" ] && [ -d "$APP_REPO" ]; then
    echo "  cloning app at $REF..."
    incus exec "$TMP_CT" -- git clone --depth 1 --branch "$REF" "$APP_REPO" /tmp/app-src
  fi
fi

# Run build
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
  incus image alias create --reuse "opsavor-${IMAGE_NAME}:latest" "$ALIAS" 2>/dev/null || true
fi

incus delete "$TMP_CT" --force
echo "[build] Done: $ALIAS"
incus image list "$IMAGE_NAME"