#!/bin/bash
set -euo pipefail

# Build the opsavor-base image
incus delete build-base --force 2>/dev/null || true

echo "=== Building opsavor-base ==="
incus launch images:debian/13 build-base --profile base

# Wait for container to have network
echo "Waiting for network..."
for i in $(seq 1 30); do
  if incus exec build-base -- ping -c 1 -W 2 8.8.8.8 >/dev/null 2>&1; then
    echo "Network up"
    break
  fi
  sleep 2
done

# Push and run build
incus file push /root/native-ops/images/base/build.sh build-base/tmp/build.sh
incus exec build-base -- bash /tmp/build.sh

echo "=== Publishing opsavor-base ==="
incus stop build-base
incus publish build-base --alias opsavor-base
incus image list

incus delete build-base --force
echo "=== Base image built ==="
