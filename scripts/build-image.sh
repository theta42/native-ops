#!/bin/bash
# Build and publish an Incus image from an image recipe.
#
# Usage: ./scripts/build-image.sh <image-name> [ref]
#   image-name: base | edge | manager | restaurant | gitea | plane | bookstack
#   ref:        git tag/branch for app images (unused for base/service images)
#
# Process:
#   1. Launch a temporary container from base image (or opsavor-base)
#   2. Run the image's build.sh inside it
#   3. Stop and publish as an image alias
#   4. Delete the temporary container
#
# The build runs on the Incus host. It uses full CPU/memory temporarily —
# the ci profile is not used here because this runs before CI is set up.
# For CI builds, the runner container itself has the Incus socket mounted.
set -euo pipefail

IMAGE_NAME="${1:?usage: build-image.sh <name> [ref]}"
REF="${2:-}"
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_DIR="$REPO_DIR/images/$IMAGE_NAME"

[ -d "$BUILD_DIR" ] || { echo "no image recipe at $BUILD_DIR" >&2; exit 1; }
[ -f "$BUILD_DIR/build.sh" ] || { echo "no build.sh in $BUILD_DIR" >&2; exit 1; }

TMP_CT="build-${IMAGE_NAME}-$$"
ALIAS="opsavor-${IMAGE_NAME}"
[ -n "$REF" ] && ALIAS="${ALIAS}:${REF}"

# Select the base image to build FROM
case "$IMAGE_NAME" in
  base)
    FROM="images:debian/13"
    ;;
  manager|restaurant)
    FROM="opsavor-base"
    ;;
  *)
    FROM="opsavor-base"
    ;;
esac

echo "[build] Building $ALIAS from $FROM (ref: ${REF:-none})"

# Launch temp container
incus launch "$FROM" "$TMP_CT" --profile base --profile ci
sleep 2

# Push the build context
build_ctx="$BUILD_DIR"
incus file push -r "$build_ctx/" "$TMP_CT/tmp/build/"

# Run the build
if [ -n "$REF" ]; then
  incus exec "$TMP_CT" -- bash /tmp/build/build.sh "$REF"
else
  incus exec "$TMP_CT" -- bash /tmp/build/build.sh
fi

# Stop and publish
incus stop "$TMP_CT"
incus publish "$TMP_CT" --alias "$ALIAS" --reuse

# Track latest for app images
if [ -n "$REF" ]; then
  incus image alias create --reuse "opsavor-${IMAGE_NAME}:latest" "$ALIAS" 2>/dev/null || true
fi

# Cleanup
incus delete "$TMP_CT"

echo "[build] Done: $ALIAS"
incus image list "$IMAGE_NAME"
