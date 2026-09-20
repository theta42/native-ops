#!/bin/bash
# Deploy the manager: build its image, replace the running container, health-gate.
# Replaces edge/deploy-manager.sh from do-ops.
#
# Usage: ./scripts/deploy-manager.sh [ref]
# Called by the Gitea Actions runner, or manually from the Incus host.
set -euo pipefail

REF="${1:-refs/heads/main}"
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
ENV_FILE="${ENV_FILE:-/root/.env}"

# Source secrets
set -a; [ -f "$ENV_FILE" ] && . "$ENV_FILE"; set +a

MANAGER_REPO="https://git.opsavor.app/opsavor/management.git"
TAG_NAME="$(echo "$REF" | sed 's|refs/tags/||;s|refs/heads/||')"
IMAGE_ALIAS="opsavor-manager:${TAG_NAME}"

echo "[deploy] Deploying manager at $REF"

# 1. Clone and build image
rm -rf /tmp/manager-build
git clone --depth 1 --branch "$TAG_NAME" "$MANAGER_REPO" /tmp/manager-build

# Build the image
"$REPO_DIR/scripts/build-image.sh" manager "$TAG_NAME"

# 2. Snapshot data volume before replacing
if incus info manager >/dev/null 2>&1; then
  echo "[deploy] Snapshotting manager-data before update..."
  incus snapshot create manager-data "pre-update-$(date +%Y%m%d-%H%M%S)" 2>/dev/null || true
fi

# 3. Stop and remove old container
if incus info manager >/dev/null 2>&1; then
  echo "[deploy] Replacing manager container..."
  incus stop manager
  incus delete manager
fi

# 4. Launch new container
incus launch "$IMAGE_ALIAS" manager \
  --profile base \
  --config "environment.MANAGER_TOKEN=${MANAGER_TOKEN:-}" \
  --config "environment.OLLAMA_DEFAULT_TOKEN=${OLLAMA_DEFAULT_TOKEN:-}"

# 5. Attach data volume if it exists
if incus storage volume list default 2>/dev/null | grep -q "manager-data"; then
  incus storage volume attach default manager-data manager /app/.data
else
  incus storage volume create default manager-data
  incus storage volume attach default manager-data manager /app/.data
fi

incus start manager

# 6. Health gate
echo "[deploy] Waiting for manager to become healthy..."
for i in $(seq 1 24); do
  if incus exec manager -- curl -fsS --max-time 5 http://127.0.0.1:3001/health >/dev/null 2>&1; then
    echo "[deploy] Manager healthy @ $TAG_NAME"
    exit 0
  fi
  sleep 5
done

echo "[deploy] Manager did not become healthy" >&2
exit 1
