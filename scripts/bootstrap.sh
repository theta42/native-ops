#!/bin/bash
# Build the opsavor-base image on the Incus host.
# Run from the Incus host itself: bash /root/native-ops/scripts/bootstrap.sh
set -euo pipefail

echo "=== Building opsavor-base image ==="

# Clean up any previous build
incus delete build-base --force 2>/dev/null || true

incus launch images:debian/13 build-base --profile base

# Wait for container to be ready
echo "Waiting for container to start..."
for i in $(seq 1 30); do
  if incus exec build-base -- systemctl is-system-running >/dev/null 2>&1; then
    break
  fi
  sleep 2
done

# Push and run the base build script
incus file push /root/native-ops/images/base/build.sh build-base/tmp/build.sh
incus exec build-base -- bash /tmp/build.sh

echo "=== Publishing opsavor-base ==="
incus stop build-base
incus publish build-base --alias opsavor-base
incus image list

# Cleanup
incus delete build-base --force

echo "=== Base image built and published ==="
