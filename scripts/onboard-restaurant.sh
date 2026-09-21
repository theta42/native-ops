#!/bin/bash
# Onboard a restaurant: create container, volume, configure, health-gate.
#
# Usage: ./scripts/onboard-restaurant.sh <slug> <name> <owner-email> <owner-password> [size]
# This replaces the DO droplet-per-restaurant model with an Incus LXC container.
#
# Expected to be called by the manager (future) or manually for now.
set -euo pipefail

SLUG="${1:?usage: onboard-restaurant.sh <slug> <name> <owner-email> <owner-password> [size]}"
NAME="${2:?}"
OWNER_EMAIL="${3:?}"
OWNER_PASSWORD="${4:?}"
SIZE="${5:-medium}"

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
source "$REPO_DIR/scripts/lib.sh"
set -a; [ -f /root/.env ] && . /root/.env; set +a

# Validate slug (lowercase alphanumeric + hyphens, 2-30 chars)
if ! echo "$SLUG" | grep -qE '^[a-z0-9-]{2,30}$'; then
  echo "invalid slug: $SLUG (use lowercase letters, digits, hyphens; 2-30 chars)" >&2
  exit 1
fi

# Check if container already exists
if incus info "rest-$SLUG" >/dev/null 2>&1; then
  echo "restaurant $SLUG already exists" >&2
  exit 1
fi

# Get the restaurant image
FP=$(image_fingerprint "opsavor-restaurant:latest" || true)
if [ -z "$FP" ]; then
  echo "no opsavor-restaurant image found. Build it first:" >&2
  echo "  ./scripts/build-image.sh restaurant restaurant-v0.2.2" >&2
  exit 1
fi

# Size mapping (CPU/memory)
case "$SIZE" in
  small)  CPU=1; MEM="512MB" ;;
  medium) CPU=1; MEM="1GB" ;;
  large)  CPU=2; MEM="2GB" ;;
  *)      CPU=1; MEM="1GB" ;;
esac

echo "[onboard] Creating restaurant: $SLUG"

# Create volume
incus storage volume create default "rest-$SLUG-data" 2>/dev/null || true
VOL_PATH="/var/lib/incus/storage-pools/default/custom/default_rest-$SLUG-data"
mkdir -p "$VOL_PATH"
chmod 777 "$VOL_PATH"

# Launch container
incus launch "$FP" "rest-$SLUG" --profile base --profile restaurant \
  --config "limits.cpu=$CPU" \
  --config "limits.memory=$MEM"

sleep 3

# Attach the volume BEFORE writing the env: the systemd unit reads
# EnvironmentFile=-/app/.data/env, which only lands on the persistent volume
# if that path is already the mount when we write to it. Writing first and
# attaching after would put the file on the container's ephemeral rootfs
# copy of /app/.data, which the device-add then shadows — silently losing it.
incus config device add "rest-$SLUG" data disk source="$VOL_PATH" path=/app/.data

SERVICE_TOKEN=$(openssl rand -hex 32)
incus exec "rest-$SLUG" -- bash -c "cat > /app/.data/env <<EOF
SITE_SLUG=$SLUG
BASE_URL=https://${SLUG}.opsavor.app
OWNER_EMAIL=$OWNER_EMAIL
OWNER_PASSWORD=$OWNER_PASSWORD
SERVICE_TOKEN=$SERVICE_TOKEN
OLLAMA_API_KEY=${OLLAMA_DEFAULT_TOKEN:-}
OLLAMA_MODEL=${OLLAMA_DEFAULT_MODEL:-gemma4:31b-cloud}
EOF"

incus restart "rest-$SLUG"

# Health gate
echo "[onboard] Waiting for $SLUG to become healthy..."
for i in $(seq 1 36); do
  if incus exec "rest-$SLUG" -- curl -fsS --max-time 5 http://127.0.0.1:3000/api/health >/dev/null 2>&1; then
    echo "[onboard] $SLUG is healthy"
    break
  fi
  if [ $i -eq 36 ]; then
    echo "[onboard] $SLUG did not become healthy" >&2
    incus exec "rest-$SLUG" -- journalctl -u restaurant --no-pager -n 20 2>/dev/null || true
    exit 1
  fi
  sleep 5
done

# Write Caddy site block. This is a plain host path (edge/sites is bind-
# mounted into the edge container as /etc/caddy/sites), not an Incus
# instance path — `incus file push` takes an <instance>/<path> destination,
# so a local heredoc redirect is what's actually needed here.
mkdir -p "$REPO_DIR/edge/sites"
SITE_FILE="$REPO_DIR/edge/sites/rest-$SLUG.caddy"
cat > "$SITE_FILE" <<CADDYEOF
${SLUG}.opsavor.app {
	tls {
		dns digitalocean {env.DO_API_TOKEN}
	}
	import strip-forged-identity
	reverse_proxy rest-${SLUG}:3000
}
CADDYEOF

# Reload edge (from the edge container)
incus exec edge -- caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile

echo "[onboard] $SLUG live at https://${SLUG}.opsavor.app"
echo "[onboard] Service token: $SERVICE_TOKEN"

# Output for automation
cat <<JSON
{
  "slug": "$SLUG",
  "url": "https://${SLUG}.opsavor.app",
  "service_token": "$SERVICE_TOKEN",
  "container": "rest-$SLUG",
  "volume": "rest-$SLUG-data"
}
JSON