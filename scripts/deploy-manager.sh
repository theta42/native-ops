#!/bin/bash
# Deploy the manager: build image from a repo checkout, replace the running
# container, health-gate. Called by the Gitea Actions runner or manually.
#
# Usage: ./scripts/deploy-manager.sh [ref]
#   ref: git tag/branch in the management repo. Defaults to current HEAD.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
MGMT_REPO="${MGMT_REPO:-/root/management}"
ENV_FILE="${ENV_FILE:-/root/.env}"
MANAGER_FINGERPRINT=""

# Source secrets
set -a
[ -f "$ENV_FILE" ] && . "$ENV_FILE"
set +a

REF="${1:-}"
if [ -z "$REF" ] && [ -d "$MGMT_REPO" ]; then
  REF=$(git -C "$MGMT_REPO" describe --tags --always --dirty 2>/dev/null || echo "dev")
fi
[ -n "$REF" ] || { echo "no ref given and no management repo found" >&2; exit 1; }

echo "[deploy] Deploying manager at $REF"

# 1. Ensure base image exists
if ! incus image alias list | grep -q "opsavor-base"; then
  echo "[deploy] Building opsavor-base..."
  "$REPO_DIR/scripts/build-base.sh"
fi

# 2. Build the manager image
TMP_CT="build-manager-$$"
incus delete "$TMP_CT" --force 2>/dev/null || true
incus launch opsavor-base "$TMP_CT" --profile base

echo "  waiting for network..."
for i in $(seq 1 30); do
  if incus exec "$TMP_CT" -- ping -c 1 -W 2 8.8.8.8 >/dev/null 2>&1; then
    break
  fi
  sleep 2
done

# Push and build
incus file push -r "$MGMT_REPO/" "$TMP_CT/app/"
incus exec "$TMP_CT" -- bash /app/build.sh "$REF" <<'BUILDSH'
#!/bin/bash
set -euo pipefail
cd /app
npm ci --production 2>/dev/null || npm install --production

useradd --system --shell /usr/sbin/nologin manager 2>/dev/null || true
mkdir -p /app/.data
chown -R manager:manager /app

cat > /etc/systemd/system/manager.service <<'SVC'
[Unit]
Description=Opsavor fleet manager
After=network.target

[Service]
User=manager
Group=manager
WorkingDirectory=/app
ExecStart=/usr/bin/node server.mjs
Restart=on-failure
RestartSec=5
EnvironmentFile=-/etc/default/manager

[Install]
WantedBy=multi-user.target
SVC

systemctl enable manager

apt-get clean
BUILDSH

incus stop "$TMP_CT"
IMAGE_ALIAS="opsavor-manager:${REF}"
incus publish "$TMP_CT" --alias "$IMAGE_ALIAS" --reuse
incus image alias create --reuse opsavor-manager:latest "${IMAGE_ALIAS}" 2>/dev/null || true
incus delete "$TMP_CT" --force
echo "[deploy] Published $IMAGE_ALIAS"

# 3. Snapshot data volume before replacing
if incus info manager >/dev/null 2>&1; then
  echo "[deploy] Snapshotting manager-data..."
  incus snapshot create manager-data "pre-update-$(date +%Y%m%d-%H%M%S)" 2>/dev/null || true
fi

# 4. Replace container
incus delete manager --force 2>/dev/null || true
incus storage volume delete default manager-data 2>/dev/null || true
incus storage volume create default manager-data
VOLUME_PATH="/var/lib/incus/storage-pools/default/custom/default_manager-data"
mkdir -p "$VOLUME_PATH"
chmod 777 "$VOLUME_PATH"

# The image fingerprint (avoid colon issues in alias parsing)
FP=$(incus image alias list | awk '/opsavor-manager:latest/ {print $2}')
if [ -z "$FP" ]; then
  FP=$(incus image list opsavor-manager | awk 'NR==3{print $2}')
fi

incus launch "$FP" manager --profile base --profile service

sleep 3

# Write env file for systemd
incus exec manager -- bash -c "cat > /etc/default/manager <<'EOF'
MANAGER_TOKEN=${MANAGER_TOKEN}
OLLAMA_DEFAULT_TOKEN=${OLLAMA_DEFAULT_TOKEN:-}
OLLAMA_DEFAULT_MODEL=${OLLAMA_DEFAULT_MODEL:-gemma4:31b-cloud}
FLEET_DB=/app/.data/fleet.db
CADDY_SITES_DIR=/sites
EOF
chown manager:manager /etc/default/manager"

# Attach data volume
incus config device add manager data disk source="$VOLUME_PATH" path=/app/.data

# Mount sites dir (shared with edge container)
incus config device add manager sites disk source="$REPO_DIR/edge/sites" path=/sites

incus restart manager

# 5. Health gate
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