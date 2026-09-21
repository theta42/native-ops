#!/bin/bash
# Deploy Outline (outline.opsavor.work), replacing Wiki.js as the internal
# wiki. Like Wiki.js and Plane, this runs the upstream OCI image directly
# via Incus rather than building from source.
#
# Requires Postgres + Redis (both provisioned here, same pattern as
# deploy-wikijs.sh/deploy-plane.sh) and Google OAuth credentials
# (GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET) already in /root/.env — the same
# shared "Internal" Google Cloud OAuth client used for the manager, Gitea,
# and Wiki.js. Google Cloud Console needs
# https://outline.opsavor.work/auth/google.callback added to that client's
# authorized redirect URIs by hand first (no API for that step); Outline
# comes up fine without it, Google sign-in just won't work until it's done.
#
# Usage: ./scripts/deploy-outline.sh
set -euo pipefail

ENV_FILE="${ENV_FILE:-/root/.env}"
set -a
[ -f "$ENV_FILE" ] && . "$ENV_FILE"
set +a
: "${GOOGLE_CLIENT_ID:?GOOGLE_CLIENT_ID must be set in $ENV_FILE}"
: "${GOOGLE_CLIENT_SECRET:?GOOGLE_CLIENT_SECRET must be set in $ENV_FILE}"

incus remote list --format csv | grep -q '^docker,' || incus remote add docker https://docker.io --protocol=oci

SECRETS="/root/.outline-secrets.env"
if [ ! -f "$SECRETS" ]; then
  echo "[deploy] generating Outline secrets..."
  {
    echo "DB_PASS=$(openssl rand -hex 24)"
    echo "SECRET_KEY=$(openssl rand -hex 32)"
    echo "UTILS_SECRET=$(openssl rand -hex 32)"
  } > "$SECRETS"
  chmod 600 "$SECRETS"
fi
set -a
# shellcheck disable=SC1090
. "$SECRETS"
set +a

new_volume() {
  incus storage volume create default "$1" 2>/dev/null || true
  incus storage volume set default "$1" security.shifted=true
}

# Postgres/Redis are stateful infra, created once and left alone on
# redeploys — same reasoning as plane-db/plane-redis in deploy-plane.sh.
skip_if_exists() {
  incus info "$1" >/dev/null 2>&1
}

get_ip() {
  incus list "$1" --format json | python3 -c "
import json, sys
rows = json.load(sys.stdin)
addrs = rows[0].get('state', {}).get('network', {}).get('eth0', {}).get('addresses', [])
print(next(a['address'] for a in addrs if a['family'] == 'inet'))
"
}

echo "[deploy] Postgres..."
if skip_if_exists outline-db; then
  echo "  outline-db already exists, leaving it alone"
else
  new_volume outline-db-data
  incus launch docker:postgres:15.7-alpine outline-db --profile base \
    --config environment.POSTGRES_USER=outline \
    --config environment.POSTGRES_PASSWORD="$DB_PASS" \
    --config environment.POSTGRES_DB=outline
  sleep 5
  incus config device add outline-db data disk pool=default source=outline-db-data path=/var/lib/postgresql/data
  incus restart outline-db
fi

echo "[deploy] Redis (valkey)..."
if skip_if_exists outline-redis; then
  echo "  outline-redis already exists, leaving it alone"
else
  new_volume outline-redis-data
  incus launch docker:valkey/valkey:7.2.11-alpine outline-redis --profile base
  incus config device add outline-redis data disk pool=default source=outline-redis-data path=/data
  incus restart outline-redis
fi

OUTLINE_DB_IP="$(get_ip outline-db)"
OUTLINE_REDIS_IP="$(get_ip outline-redis)"

new_volume outline-data
if incus info outline >/dev/null 2>&1; then
  echo "[deploy] Snapshotting outline-data before replace..."
  incus storage volume snapshot create default outline-data "pre-update-$(date +%Y%m%d-%H%M%S)" 2>/dev/null || true
fi

echo "[deploy] Outline..."
incus delete outline --force 2>/dev/null || true
incus launch docker:outlinewiki/outline:1.10.1 outline --profile base \
  --config limits.cpu=2 --config limits.memory=1GB \
  --config environment.NODE_ENV=production \
  --config environment.URL=https://outline.opsavor.work \
  --config environment.PORT=3000 \
  --config environment.SECRET_KEY="$SECRET_KEY" \
  --config environment.UTILS_SECRET="$UTILS_SECRET" \
  --config environment.DATABASE_URL="postgres://outline:${DB_PASS}@${OUTLINE_DB_IP}:5432/outline" \
  --config environment.PGSSLMODE=disable \
  --config environment.REDIS_URL="redis://${OUTLINE_REDIS_IP}:6379" \
  --config environment.FILE_STORAGE=local \
  --config environment.FILE_STORAGE_LOCAL_ROOT_DIR=/var/lib/outline/data \
  --config environment.GOOGLE_CLIENT_ID="$GOOGLE_CLIENT_ID" \
  --config environment.GOOGLE_CLIENT_SECRET="$GOOGLE_CLIENT_SECRET" \
  --config environment.FORCE_HTTPS=false

sleep 3
incus config device add outline data disk pool=default source=outline-data path=/var/lib/outline/data
# The image's own entrypoint runs as uid/gid 1001 (its "nodejs" user, not
# the 1000 other upstream Node-based images here use) — chown after attach
# so it can actually write to the volume (security.shifted makes this uid
# mapping meaningful; without the chown the freshly-attached dir is still
# container-root-owned).
incus exec outline -- chown -R 1001:1001 /var/lib/outline/data
incus restart outline

echo "[deploy] Waiting for Outline to become healthy (first boot runs DB migrations)..."
for i in $(seq 1 30); do
  OUTLINE_IP="$(get_ip outline 2>/dev/null || true)"
  if [ -n "$OUTLINE_IP" ] && curl -fsS --max-time 5 "http://${OUTLINE_IP}:3000/_health" 2>/dev/null | grep -q "OK"; then
    echo "[deploy] Outline healthy"
    exit 0
  fi
  sleep 5
done

echo "[deploy] Outline did not become healthy" >&2
incus console outline --show-log 2>&1 | tail -40
exit 1
