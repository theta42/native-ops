#!/bin/bash
# Deploy Plane (tickets.opsavor.work). Unlike every other service in this
# fleet, this does NOT build from source: Plane is a genuinely large
# multi-service stack (Django API + Celery worker + beat scheduler + three
# separate Next.js frontends + a realtime collaboration server), and
# rebuilding all of that natively would take far longer and be far more
# fragile than using Plane's own maintained images. Incus can pull and run
# OCI images directly (no separate Docker daemon needed — still holds to
# AGENTS.md principle 3 in spirit), so this runs Plane's official
# All-In-One image (makeplane/plane-aio-community, which bundles web/
# space/admin/api/live/worker/beat/an internal Caddy into one container)
# plus Postgres/Redis/RabbitMQ/MinIO as its four required external
# services, all as Incus OCI application containers on the same
# incusbr0 bridge (DNS-resolvable by name, same as the rest of the fleet).
#
# Usage: ./scripts/deploy-plane.sh [domain]
set -euo pipefail

incus remote list --format csv | grep -q '^docker,' || incus remote add docker https://docker.io --protocol=oci
incus remote list --format csv | grep -q '^quay,' || incus remote add quay https://quay.io --protocol=oci

PLANE_DOMAIN="${1:-tickets.opsavor.work}"
SECRETS="/root/.plane-secrets.env"

if [ ! -f "$SECRETS" ]; then
  echo "[deploy] generating Plane secrets..."
  {
    echo "POSTGRES_PASSWORD=$(openssl rand -hex 24)"
    echo "RABBITMQ_PASSWORD=$(openssl rand -hex 24)"
    echo "MINIO_ROOT_USER=$(openssl rand -hex 12)"
    echo "MINIO_ROOT_PASSWORD=$(openssl rand -hex 24)"
    echo "SECRET_KEY=$(openssl rand -hex 32)"
    echo "LIVE_SERVER_SECRET_KEY=$(openssl rand -hex 32)"
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

echo "[deploy] Postgres..."
new_volume plane-db-data
incus delete plane-db --force 2>/dev/null || true
incus launch docker:postgres:15.7-alpine plane-db --profile base \
  --config environment.POSTGRES_USER=plane \
  --config environment.POSTGRES_PASSWORD="$POSTGRES_PASSWORD" \
  --config environment.POSTGRES_DB=plane
sleep 5
incus config device add plane-db data disk pool=default source=plane-db-data path=/var/lib/postgresql/data
incus restart plane-db

echo "[deploy] Redis (valkey)..."
new_volume plane-redis-data
incus delete plane-redis --force 2>/dev/null || true
incus launch docker:valkey/valkey:7.2.11-alpine plane-redis --profile base
sleep 5
incus config device add plane-redis data disk pool=default source=plane-redis-data path=/data
incus restart plane-redis

echo "[deploy] RabbitMQ..."
new_volume plane-mq-data
incus delete plane-mq --force 2>/dev/null || true
incus launch docker:rabbitmq:3.13.6-management-alpine plane-mq --profile base \
  --config environment.RABBITMQ_DEFAULT_USER=plane \
  --config environment.RABBITMQ_DEFAULT_PASS="$RABBITMQ_PASSWORD" \
  --config environment.RABBITMQ_DEFAULT_VHOST=plane
sleep 5
incus config device add plane-mq data disk pool=default source=plane-mq-data path=/var/lib/rabbitmq
incus restart plane-mq

echo "[deploy] MinIO..."
# minio/minio on Docker Hub now returns "requested access to the resource
# is denied" outright (MinIO moved off Docker Hub at some point) — quay.io
# still serves it.
new_volume plane-minio-data
incus delete plane-minio --force 2>/dev/null || true
incus launch quay:minio/minio plane-minio --profile base \
  --config environment.MINIO_ROOT_USER="$MINIO_ROOT_USER" \
  --config environment.MINIO_ROOT_PASSWORD="$MINIO_ROOT_PASSWORD" \
  --config environment.MINIO_VOLUMES=/export \
  --config environment.MINIO_CONSOLE_ADDRESS=:9090
sleep 5
incus config device add plane-minio data disk pool=default source=plane-minio-data path=/export
incus restart plane-minio

echo "[deploy] waiting for MinIO..."
for i in $(seq 1 24); do
  incus exec plane-minio -- curl -fsS --max-time 3 http://127.0.0.1:9000/minio/health/live >/dev/null 2>&1 && break
  sleep 5
done

echo "[deploy] ensuring the uploads bucket exists..."
incus delete plane-mc-init --force 2>/dev/null || true
incus launch quay:minio/mc plane-mc-init --profile base --ephemeral \
  --config environment.MC_HOST_local="http://${MINIO_ROOT_USER}:${MINIO_ROOT_PASSWORD}@plane-minio:9000"
sleep 3
incus exec plane-mc-init -- mc mb --ignore-existing local/uploads
incus delete plane-mc-init --force 2>/dev/null || true

echo "[deploy] Plane (all-in-one)..."
# No `latest` tag exists for this image ("manifest unknown") — pin an
# actual release tag instead.
incus delete plane --force 2>/dev/null || true
incus launch docker:makeplane/plane-aio-community:v1.4.2 plane --profile base \
  --config environment.DOMAIN_NAME="$PLANE_DOMAIN" \
  --config environment.DATABASE_URL="postgresql://plane:${POSTGRES_PASSWORD}@plane-db:5432/plane" \
  --config environment.REDIS_URL="redis://plane-redis:6379/" \
  --config environment.AMQP_URL="amqp://plane:${RABBITMQ_PASSWORD}@plane-mq:5672/plane" \
  --config environment.AWS_REGION=us-east-1 \
  --config environment.AWS_ACCESS_KEY_ID="$MINIO_ROOT_USER" \
  --config environment.AWS_SECRET_ACCESS_KEY="$MINIO_ROOT_PASSWORD" \
  --config environment.AWS_S3_BUCKET_NAME=uploads \
  --config environment.AWS_S3_ENDPOINT_URL=http://plane-minio:9000 \
  --config environment.USE_MINIO=1 \
  --config environment.SITE_ADDRESS=:80 \
  --config environment.SECRET_KEY="$SECRET_KEY" \
  --config environment.LIVE_SERVER_SECRET_KEY="$LIVE_SERVER_SECRET_KEY"

echo "[deploy] Waiting for Plane to become healthy (first boot runs DB migrations across several services — can take a few minutes)..."
for i in $(seq 1 60); do
  if incus exec plane -- curl -fsS --max-time 5 http://127.0.0.1/ >/dev/null 2>&1; then
    echo "[deploy] Plane healthy"
    exit 0
  fi
  sleep 5
done

echo "[deploy] Plane did not become healthy" >&2
exit 1
