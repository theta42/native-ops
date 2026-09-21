#!/bin/bash
# Deploy Wiki.js (wiki.opsavor.work), replacing BookStack. Like Plane, this
# runs upstream OCI images directly via Incus rather than building from
# source — Wiki.js's own Docker image is the maintained, documented way to
# run it, and there's no benefit to reimplementing that.
#
# Usage: ./scripts/deploy-wikijs.sh
set -euo pipefail

incus remote list --format csv | grep -q '^docker,' || incus remote add docker https://docker.io --protocol=oci

SECRETS="/root/.wikijs-secrets.env"
if [ ! -f "$SECRETS" ]; then
  echo "[deploy] generating Wiki.js secrets..."
  echo "DB_PASS=$(openssl rand -hex 24)" > "$SECRETS"
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

# Postgres is stateful infra, created once and left alone on redeploys —
# same reasoning as plane-db in deploy-plane.sh.
skip_if_exists() {
  incus info "$1" >/dev/null 2>&1
}

echo "[deploy] Postgres..."
if skip_if_exists wikijs-db; then
  echo "  wikijs-db already exists, leaving it alone"
else
  new_volume wikijs-db-data
  incus launch docker:postgres:15.7-alpine wikijs-db --profile base \
    --config environment.POSTGRES_USER=wikijs \
    --config environment.POSTGRES_PASSWORD="$DB_PASS" \
    --config environment.POSTGRES_DB=wiki
  sleep 5
  incus config device add wikijs-db data disk pool=default source=wikijs-db-data path=/var/lib/postgresql/data
  incus restart wikijs-db
fi

get_ip() {
  incus list "$1" --format json | python3 -c "
import json, sys
rows = json.load(sys.stdin)
addrs = rows[0].get('state', {}).get('network', {}).get('eth0', {}).get('addresses', [])
print(next(a['address'] for a in addrs if a['family'] == 'inet'))
"
}
WIKIJS_DB_IP="$(get_ip wikijs-db)"

new_volume wikijs-data
if incus info wikijs >/dev/null 2>&1; then
  echo "[deploy] Snapshotting wikijs-data before replace..."
  incus storage volume snapshot create default wikijs-data "pre-update-$(date +%Y%m%d-%H%M%S)" 2>/dev/null || true
fi

echo "[deploy] Wiki.js..."
incus delete wikijs --force 2>/dev/null || true
incus launch docker:requarks/wiki:2 wikijs --profile base \
  --config environment.DB_TYPE=postgres \
  --config environment.DB_HOST="$WIKIJS_DB_IP" \
  --config environment.DB_PORT=5432 \
  --config environment.DB_USER=wikijs \
  --config environment.DB_PASS="$DB_PASS" \
  --config environment.DB_NAME=wiki

sleep 3
incus config device add wikijs data disk pool=default source=wikijs-data path=/wiki/data/content
# The upstream image's entrypoint runs as uid 1000 (the "node" user baked
# into the base Node image) — chown after attach so it can actually write
# uploaded assets to the volume (security.shifted makes this uid mapping
# meaningful; without the chown the freshly-attached dir is still
# container-root-owned).
incus exec wikijs -- chown -R 1000:1000 /wiki/data/content
incus restart wikijs

echo "[deploy] Waiting for Wiki.js to become healthy (first boot runs DB migrations)..."
# The upstream image doesn't ship curl/wget, so check from the host against
# the container's own IP instead of execing a probe inside it.
for i in $(seq 1 24); do
  WIKIJS_IP="$(get_ip wikijs 2>/dev/null || true)"
  if [ -n "$WIKIJS_IP" ] && curl -fsS --max-time 5 "http://${WIKIJS_IP}:3000/" >/dev/null 2>&1; then
    echo "[deploy] Wiki.js healthy"
    exit 0
  fi
  sleep 5
done

echo "[deploy] Wiki.js did not become healthy" >&2
incus console wikijs --show-log 2>&1 | tail -40
exit 1
