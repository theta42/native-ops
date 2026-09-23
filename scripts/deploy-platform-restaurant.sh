#!/bin/bash
# Deploy a REAL restaurant instance of opsavor/platform.
#
# Usage: ./scripts/deploy-platform-restaurant.sh <slug> "<name>" <owner-email> [dump-dir] [ref]
#   slug:       tenant slug + hostname (sicily -> sicily.opsavor.app, container rest-sicily)
#   name:       display name ("Sicily Coal Fired Pizza")
#   owner-email: the real owner login (no shared demo accounts)
#   dump-dir:   optional host path of real artifacts to ingest (raw-first) at seed
#   ref:        image ref (default latest)
#
# Unlike deploy-platform-demo.sh this creates a persistent data volume
# (rest-<slug>-data at /app/.data), seeds a real tenant with only an owner
# account, and ingests the given artifacts through the normal pipeline.
set -euo pipefail

SLUG="${1:?usage: deploy-platform-restaurant.sh <slug> \"<name>\" <owner-email> [dump-dir] [ref]}"
NAME_DISPLAY="${2:?display name required}"
OWNER_EMAIL="${3:?owner email required}"
DUMP_DIR="${4:-}"
REF="${5:-latest}"
PORT=8787
CT="rest-${SLUG}"
VOL="rest-${SLUG}-data"

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
source "$REPO_DIR/scripts/lib.sh"
ALIAS="opsavor-platform:${REF}"
FP="$(image_fingerprint "$ALIAS")" || { echo "image $ALIAS not found — run: build-image.sh platform <ref>" >&2; exit 1; }

if ! echo "$SLUG" | grep -qE '^[a-z0-9-]{2,30}$'; then echo "invalid slug: $SLUG" >&2; exit 1; fi

echo "[deploy] $CT ($NAME_DISPLAY) <- $ALIAS ($FP)"
incus delete "$CT" --force 2>/dev/null || true
incus storage volume create default "$VOL" 2>/dev/null || true
incus launch "$FP" "$CT" --profile base --profile service \
  --config limits.cpu=1 --config limits.memory=1GB

echo "  waiting for network..."
for _ in $(seq 1 30); do incus exec "$CT" -- ping -c1 -W2 8.8.8.8 >/dev/null 2>&1 && break; sleep 2; done

# Attach the volume BEFORE anything writes under /app/.data (see AGENTS.md).
incus config device add "$CT" data disk pool=default source="$VOL" path=/app/.data

CTRL_TOKEN="$(openssl rand -hex 16)"
ENVF="$(mktemp)"
cat > "$ENVF" <<EOF
PORT=${PORT}
HOST=0.0.0.0
OPSAVOR_DATA=/app/.data
OPSAVOR_SEED=none
OPSAVOR_CONTROL_TOKEN=${CTRL_TOKEN}
EOF
incus file push "$ENVF" "$CT/etc/default/platform"
incus exec "$CT" -- chmod 600 /etc/default/platform
rm -f "$ENVF"

OWNER_PW="${OPSAVOR_OWNER_PASSWORD:-$(openssl rand -base64 18 | tr -dc 'A-Za-z0-9' | head -c 16)}"

# Create the real tenant + owner (and ingest artifacts if given) before the
# service starts, so first boot sees them.
if [ -n "$DUMP_DIR" ]; then
  echo "  ingesting real artifacts from $DUMP_DIR ..."
  incus exec "$CT" -- mkdir -p /root/dump
  incus file push -r "$DUMP_DIR/." "$CT/root/dump/"
  incus exec "$CT" -- env OPSAVOR_DATA=/app/.data node /app/src/cli.mjs seed \
    --dump-dir /root/dump --slug "$SLUG" --name "$NAME_DISPLAY" \
    --owner "$OWNER_EMAIL" --owner-name "Owner" --owner-password "$OWNER_PW" --real
  incus exec "$CT" -- rm -rf /root/dump
else
  incus exec "$CT" -- env OPSAVOR_DATA=/app/.data node /app/src/cli.mjs provision \
    "$SLUG" "$NAME_DISPLAY" --owner "$OWNER_EMAIL" --password "$OWNER_PW"
fi

incus exec "$CT" -- systemctl enable --now platform

IP="$(container_ip "$CT")"
echo "  health-gating http://$IP:${PORT}/health ..."
for _ in $(seq 1 45); do curl -fsS "http://$IP:${PORT}/health" >/dev/null 2>&1 && break; sleep 2; done
if ! curl -fsS "http://$IP:${PORT}/health" >/dev/null; then
  echo "[deploy] health check FAILED for $CT" >&2
  incus exec "$CT" -- journalctl -u platform -n 40 --no-pager 2>&1 || true
  exit 1
fi

SITE="$(mktemp)"
cat > "$SITE" <<EOF
${SLUG}.opsavor.app {
	tls {
		dns digitalocean {env.DO_API_TOKEN}
	}
	import strip-forged-identity
	reverse_proxy ${CT}:${PORT}
}
EOF
incus exec edge -- mkdir -p /etc/caddy/sites
incus file push "$SITE" "edge/etc/caddy/sites/${CT}.caddy"
rm -f "$SITE"
incus exec edge -- caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
incus exec edge -- caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile

echo "[deploy] $CT ready"
echo "  url:     https://${SLUG}.opsavor.app/app/"
echo "  login:   ${OWNER_EMAIL} / ${OWNER_PW}"
echo "  control: https://${SLUG}.opsavor.app/control/v1/health  (X-Control-Token: ${CTRL_TOKEN})"
echo "  volume:  ${VOL} -> /app/.data"
