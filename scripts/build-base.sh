#!/bin/bash
# Build and publish the base image (<prefix>base) on the Incus host, from images/base/build.sh.
# The prefix is $NATIVE_OPS_IMAGE_PREFIX, default "app-" (see scripts/build-image.sh).
# Run on the Incus host from a checkout of the config repo: bash scripts/build-base.sh
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
exec bash "$REPO_DIR/scripts/build-image.sh" base
