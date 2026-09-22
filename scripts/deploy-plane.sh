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

# Postgres/Redis/RabbitMQ/MinIO are stateful infrastructure, not something
# this script ever needs to "update" the way the Plane app container below
# gets replaced on every run — force-deleting and relaunching them
# unconditionally on every re-run (as an earlier version of this script
# did) sends a running Postgres a hard kill instead of a graceful
# shutdown, and doing that across several iterations while debugging
# corrupted its data directory for real ("could not locate a valid
# checkpoint record"). If one already exists, leave it alone entirely.
skip_if_exists() {
  incus info "$1" >/dev/null 2>&1
}

echo "[deploy] Postgres..."
if skip_if_exists plane-db; then
  echo "  plane-db already exists, leaving it alone"
else
  new_volume plane-db-data
  incus launch docker:postgres:15.7-alpine plane-db --profile base \
    --config environment.POSTGRES_USER=plane \
    --config environment.POSTGRES_PASSWORD="$POSTGRES_PASSWORD" \
    --config environment.POSTGRES_DB=plane
  sleep 5
  incus config device add plane-db data disk pool=default source=plane-db-data path=/var/lib/postgresql/data
  incus restart plane-db
fi

echo "[deploy] Redis (valkey)..."
if skip_if_exists plane-redis; then
  echo "  plane-redis already exists, leaving it alone"
else
  new_volume plane-redis-data
  incus launch docker:valkey/valkey:7.2.11-alpine plane-redis --profile base
  sleep 5
  incus config device add plane-redis data disk pool=default source=plane-redis-data path=/data
  incus restart plane-redis
fi

echo "[deploy] RabbitMQ..."
if skip_if_exists plane-mq; then
  echo "  plane-mq already exists, leaving it alone"
else
  new_volume plane-mq-data
  incus launch docker:rabbitmq:3.13.6-management-alpine plane-mq --profile base \
    --config environment.RABBITMQ_DEFAULT_USER=plane \
    --config environment.RABBITMQ_DEFAULT_PASS="$RABBITMQ_PASSWORD" \
    --config environment.RABBITMQ_DEFAULT_VHOST=plane
  sleep 5
  incus config device add plane-mq data disk pool=default source=plane-mq-data path=/var/lib/rabbitmq
  incus restart plane-mq
fi

echo "[deploy] MinIO..."
if skip_if_exists plane-minio; then
  echo "  plane-minio already exists, leaving it alone"
else
  # minio/minio on Docker Hub now returns "requested access to the
  # resource is denied" outright (MinIO moved off Docker Hub at some
  # point) — quay.io still serves it.
  # The image's own entrypoint (`/usr/bin/docker-entrypoint.sh minio` —
  # check `incus config show <instance>`'s auto-populated `oci.entrypoint`
  # on any OCI instance to see this per-image) passes bare `minio` with no
  # subcommand, which just prints usage and exits: MINIO_VOLUMES as an env
  # var is NOT enough on its own, minio still needs the `server`
  # subcommand and path as actual arguments. `oci.entrypoint` is a real,
  # overridable Incus config key for exactly this.
  new_volume plane-minio-data
  incus launch quay:minio/minio plane-minio --profile base \
    --config environment.MINIO_ROOT_USER="$MINIO_ROOT_USER" \
    --config environment.MINIO_ROOT_PASSWORD="$MINIO_ROOT_PASSWORD" \
    --config oci.entrypoint="/usr/bin/docker-entrypoint.sh minio server /export --console-address :9090"
  sleep 5
  incus config device add plane-minio data disk pool=default source=plane-minio-data path=/export
  incus restart plane-minio
fi

echo "[deploy] waiting for MinIO..."
minio_up=false
for i in $(seq 1 24); do
  if incus exec plane-minio -- curl -fsS --max-time 3 http://127.0.0.1:9000/minio/health/live >/dev/null 2>&1; then
    minio_up=true
    break
  fi
  sleep 5
done
if [ "$minio_up" != "true" ]; then
  echo "[deploy] MinIO did not become healthy" >&2
  incus console plane-minio --show-log 2>&1 | tail -30
  exit 1
fi

echo "[deploy] ensuring the uploads bucket exists..."
incus delete plane-mc-init --force 2>/dev/null || true
incus launch quay:minio/mc plane-mc-init --profile base --ephemeral \
  --config environment.MC_HOST_local="http://${MINIO_ROOT_USER}:${MINIO_ROOT_PASSWORD}@plane-minio:9000"
sleep 3
incus exec plane-mc-init -- mc mb --ignore-existing local/uploads
incus delete plane-mc-init --force 2>/dev/null || true

echo "[deploy] Plane (all-in-one)..."
# Use IPs, not hostnames, for the infra services below. This image's
# Python (musl-based) does a plain dual-stack getaddrinfo for
# DATABASE_URL/REDIS_URL/etc, and incusbr0's dnsmasq — since the network
# has no IPv6 range configured at all (ipv6.address: none) — doesn't
# answer AAAA queries with a fast empty response, it just never answers
# them, so the AAAA half of that dual-stack lookup hangs forever and the
# API/worker/beat processes sit at "wait_for_db" indefinitely (confirmed
# live: forcing socket.AF_INET resolves plane-db instantly; the default
# AF_UNSPEC call never returns). Not something to fix by changing the
# shared incusbr0 network's DNS behavior — every other container on it
# depends on that too. Resolving these four to their current IP at
# deploy time sidesteps it entirely for Plane's own connection strings.
get_ip() {
  incus list "$1" --format json | python3 -c "
import json, sys
rows = json.load(sys.stdin)
addrs = rows[0].get('state', {}).get('network', {}).get('eth0', {}).get('addresses', [])
print(next(a['address'] for a in addrs if a['family'] == 'inet'))
"
}
PLANE_DB_IP="$(get_ip plane-db)"
PLANE_REDIS_IP="$(get_ip plane-redis)"
PLANE_MQ_IP="$(get_ip plane-mq)"
# AWS_S3_ENDPOINT_URL is the public domain, not plane-minio's internal
# bridge IP — Plane bakes this address into every upload/attachment link
# it returns to the browser, so an internal-only address means every
# upload silently stalls (browser tries to POST straight to
# 10.0.100.x:9000, which it can never reach). edge/Caddyfile's /uploads/*
# route on this domain proxies that straight to plane-minio, so the same
# address works for both the browser and (via that same public hostname)
# Plane's own server-side S3 client. This was tried once before and
# reverted mid-incident because it looked implicated in an api
# crash-loop — it wasn't; that crash was the unrelated GUNICORN_WORKERS
# bug below (unset before this same file also fixed it), confirmed by the
# crash persisting even after reverting this back to the internal IP.
# Re-applied here now that the two are no longer confounded.

# No `latest` tag exists for this image ("manifest unknown") — pin an
# actual release tag instead.
#
# Google SSO: GOOGLE_CLIENT_ID/SECRET are OPERATOR-supplied, like
# bookstack's OIDC_*/gitea's GITEA_OAUTH_* — add them to
# /root/.plane-secrets.env and re-run this script (only the `plane`
# container gets replaced; the four infra services above are left alone)
# to enable it. Plane's own Google provider
# (apps/api/plane/authentication/provider/oauth/google.py) reads these
# as plain env vars directly — no god-mode/admin-UI step needed. Redirect
# URI is fixed by Plane's own code, not configurable:
# https://<domain>/auth/google/callback/
GOOGLE_OAUTH_ARGS=()
if [ -n "${GOOGLE_CLIENT_ID:-}" ]; then
  GOOGLE_OAUTH_ARGS=(
    --config environment.GOOGLE_CLIENT_ID="$GOOGLE_CLIENT_ID"
    --config environment.GOOGLE_CLIENT_SECRET="$GOOGLE_CLIENT_SECRET"
  )
fi
# The `base` profile's 512MB limit (fine for every other single-process
# service in this fleet) is nowhere near enough here: this one container
# runs ~7 processes (Django API, Celery worker, beat, 3 Next.js frontends,
# a realtime server, Caddy). Under 512MB the kernel OOM-killer inside the
# container's memcg kills api/worker/beat/migrator every few seconds
# (visible as `python manage.py wait_for_db` respawning forever with
# "Killed" in its stderr log and `dmesg -T` full of oom-kill entries for
# cpuset=lxc.payload.plane) — that looked exactly like a DB-connectivity
# hang until `dmesg` on the host made the real cause obvious. Override the
# limit at the instance level rather than raising it fleet-wide.
#
# CORS_ALLOWED_ORIGINS also matters for a reason that has nothing to do
# with CORS itself: Plane's settings.py (plane/settings/common.py) derives
# CSRF_TRUSTED_ORIGINS directly from this var, and leaves it an empty list
# when it's unset. Every GET still works fine without it, but any form
# submit (e.g. the god-mode "create instance admin" setup wizard) gets
# silently rejected by Django's CSRF check with no entry in the request
# log at all, since CSRF middleware runs ahead of Plane's own
# request-logging middleware. It also gates whether session/CSRF cookies
# get `Secure` at all (`secure_origins` in the same file), so this is the
# correct setting for an HTTPS deployment regardless.
#
# GUNICORN_WORKERS: this image's own supervisor.conf passes it straight to
# `gunicorn -w "$GUNICORN_WORKERS"` (docker-entrypoint-api.sh) with no
# default — left unset, that's `-w ""`, and gunicorn refuses to start at
# all ("error: argument -w/--workers: invalid int value: ''"), no
# traceback, api stuck in a silent restart loop. This only ever surfaces
# on a restart (confirmed live: it was wrong from this script's first
# version, invisible until the api process actually had to boot again).
incus delete plane --force 2>/dev/null || true
incus launch docker:makeplane/plane-aio-community:v1.4.2 plane --profile base \
  --config limits.memory=3GB \
  --config limits.cpu=2 \
  --config environment.DOMAIN_NAME="$PLANE_DOMAIN" \
  --config environment.DATABASE_URL="postgresql://plane:${POSTGRES_PASSWORD}@${PLANE_DB_IP}:5432/plane" \
  --config environment.REDIS_URL="redis://${PLANE_REDIS_IP}:6379/" \
  --config environment.AMQP_URL="amqp://plane:${RABBITMQ_PASSWORD}@${PLANE_MQ_IP}:5672/plane" \
  --config environment.AWS_REGION=us-east-1 \
  --config environment.AWS_ACCESS_KEY_ID="$MINIO_ROOT_USER" \
  --config environment.AWS_SECRET_ACCESS_KEY="$MINIO_ROOT_PASSWORD" \
  --config environment.AWS_S3_BUCKET_NAME=uploads \
  --config environment.AWS_S3_ENDPOINT_URL="https://${PLANE_DOMAIN}" \
  --config environment.USE_MINIO=1 \
  --config environment.SITE_ADDRESS=:80 \
  --config environment.GUNICORN_WORKERS=2 \
  --config environment.CORS_ALLOWED_ORIGINS="https://${PLANE_DOMAIN}" \
  --config environment.SECRET_KEY="$SECRET_KEY" \
  --config environment.LIVE_SERVER_SECRET_KEY="$LIVE_SERVER_SECRET_KEY" \
  "${GOOGLE_OAUTH_ARGS[@]}"

echo "[deploy] patching the vendored api entrypoint..."
# Upstream's docker-entrypoint-api.sh computes a machine-signature via
# `DISK_INFO=$(df -h)` under `set -e`. Under Incus's OCI container support,
# /sys/kernel/debug/tracing is mounted but unreadable, GNU df's exit status
# goes non-zero when it can't stat that one mount, and set -e kills the
# whole script right there — every single time this container boots, before
# ever reaching register_instance or gunicorn (confirmed live: the api
# process crash-looped with no traceback, 502 forever, until this was
# patched). Re-pushed on every deploy since it's a file inside the
# container, not something incus config set can persist across a relaunch.
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
incus file push "$REPO_DIR/images/plane/docker-entrypoint-api-patch.sh" \
  plane/app/backend/bin/docker-entrypoint-api.sh --mode 755
incus restart plane

echo "[deploy] Waiting for Plane to become healthy (first boot runs DB migrations across several services — can take a few minutes)..."
for i in $(seq 1 60); do
  # Root / is served even while the api process itself is crash-looping
  # (confirmed live — it's what let this exact outage pass the old check
  # for a full hour), so this checks /api/ instead. curl -f would treat
  # api's own normal 404-on-bare-/api/ as a failure, so this checks the
  # status code directly instead: anything but 502 (bad gateway — proxy
  # up, api down) or 000 (couldn't connect at all) means api answered.
  code="$(incus exec plane -- curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1/api/ 2>/dev/null || echo 000)"
  if [ "$code" != "502" ] && [ "$code" != "000" ]; then
    echo "[deploy] Plane healthy (api responded ${code})"
    exit 0
  fi
  sleep 5
done

echo "[deploy] Plane did not become healthy" >&2
exit 1
