#!/bin/bash
# Deploy the manager: build image from a repo checkout, replace the running
# container, health-gate. Called by the Gitea Actions runner or manually.
#
# Usage: ./scripts/deploy-manager.sh [ref]
#   ref: git tag/branch in the management repo. Defaults to "main".
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
ENV_FILE="${ENV_FILE:-/root/.env}"
source "$REPO_DIR/scripts/lib.sh"

set -a
[ -f "$ENV_FILE" ] && . "$ENV_FILE"
set +a

: "${MANAGER_TOKEN:?MANAGER_TOKEN must be set in $ENV_FILE}"

REF="${1:-main}"

echo "[deploy] Building opsavor-manager:$REF"
"$REPO_DIR/scripts/build-image.sh" manager "$REF"
FP=$(image_fingerprint "opsavor-manager:$REF")

if incus info manager >/dev/null 2>&1; then
  echo "[deploy] Snapshotting manager-data..."
  incus storage volume snapshot create default manager-data "pre-update-$(date +%Y%m%d-%H%M%S)" 2>/dev/null || true
fi

# Replace the container, but never the data volume — recreating it here
# used to wipe fleet.db on every single deploy, which is why the manager
# never actually persisted anything.
incus delete manager --force 2>/dev/null || true
incus storage volume create default manager-data 2>/dev/null || true
VOLUME_PATH="/var/lib/incus/storage-pools/default/custom/default_manager-data"
mkdir -p "$VOLUME_PATH"
chmod 777 "$VOLUME_PATH"

incus launch "$FP" manager --profile base --profile service

sleep 3

# Attach volumes BEFORE writing anything under them or restarting — a write
# to /app/.data before the device is attached lands on the container's
# ephemeral rootfs copy and is shadowed (lost) the moment the device mounts.
incus config device add manager data disk source="$VOLUME_PATH" path=/app/.data
incus config device add manager sites disk source="$REPO_DIR/edge/sites" path=/sites
mkdir -p "$REPO_DIR/edge/sites"

# Push this manager's dedicated Incus control key. The manager process has
# no Incus socket or CLI of its own inside the container; it SSHes back to
# the host as the low-privilege `manager-ctl` user (incus-admin group, no
# sudo) and runs `incus` there. See lib/incus.mjs in the management repo,
# and provision-host.sh for where manager-ctl and this key are created.
KEY_SRC="/root/.ssh/manager_incus_ed25519"
if [ ! -f "$KEY_SRC" ]; then
  echo "[deploy] $KEY_SRC missing — run ./scripts/provision-host.sh first" >&2
  exit 1
fi
incus exec manager -- mkdir -p /home/manager/.ssh
incus file push "$KEY_SRC" manager/home/manager/.ssh/incus_ctl
incus exec manager -- chown -R manager:manager /home/manager/.ssh
incus exec manager -- chmod 700 /home/manager/.ssh
incus exec manager -- chmod 600 /home/manager/.ssh/incus_ctl

# Write env file for systemd (EnvironmentFile, not `incus config set
# environment.*` — see AGENTS.md gotcha #1: the latter never reaches a
# systemd-managed process).
incus exec manager -- bash -c "cat > /etc/default/manager <<EOF
MANAGER_TOKEN=${MANAGER_TOKEN}
OLLAMA_DEFAULT_TOKEN=${OLLAMA_DEFAULT_TOKEN:-}
OLLAMA_DEFAULT_MODEL=${OLLAMA_DEFAULT_MODEL:-gemma4:31b-cloud}
FLEET_DB=/app/.data/fleet.db
CADDY_SITES_DIR=/sites
INCUS_SSH_HOST=10.0.100.1
INCUS_SSH_USER=manager-ctl
INCUS_SSH_KEY=/home/manager/.ssh/incus_ctl
EOF
chown manager:manager /etc/default/manager
chmod 600 /etc/default/manager"

incus restart manager

echo "[deploy] Waiting for manager to become healthy..."
for i in $(seq 1 24); do
  if incus exec manager -- curl -fsS --max-time 5 http://127.0.0.1:3001/health >/dev/null 2>&1; then
    echo "[deploy] Manager healthy @ $REF"
    exit 0
  fi
  sleep 5
done

echo "[deploy] Manager did not become healthy" >&2
incus exec manager -- journalctl -u manager --no-pager -n 20 2>/dev/null || true
exit 1
