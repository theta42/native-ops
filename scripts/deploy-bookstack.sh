#!/bin/bash
# Deploy BookStack: build the image, create/attach its persistent data
# volume, launch or replace the container, health-gate.
#
# Usage: ./scripts/deploy-bookstack.sh
#
# The public URL (https://wiki.opsavor.work) is baked into the entrypoint's
# default and only ever written to /data/env on the volume's FIRST boot —
# not something this script re-passes on every deploy. `incus config set
# environment.*` cannot be used for that anyway: it never reaches a
# systemd-managed process (see native-ops AGENTS.md gotcha #1). To change
# it later: edit APP_URL in /data/env inside the container, then
# `incus restart bookstack`.
#
# bookstack.service is NOT enabled in the image (see build.sh): this
# script starts it explicitly, AFTER attaching the data volume, rather
# than letting `incus launch` auto-start it against an ephemeral
# not-yet-mounted /data and then restarting moments later. That race
# actually happened — mariadb-install-db plus a backgrounded temporary
# mysqld is slow enough that the first (ephemeral) attempt was often still
# mid-flight when a subsequent `incus restart` fired, leaving two
# generations of mysqld/nginx/php-fpm alive and fighting over the same
# port/socket.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
source "$REPO_DIR/scripts/lib.sh"

echo "[deploy] Building opsavor-bookstack..."
"$REPO_DIR/scripts/build-image.sh" bookstack

incus storage volume create default bookstack-data 2>/dev/null || true
# Lets the container's own uid mapping (root, mysql, www-data — all
# container-internal) apply to this volume, so the entrypoint script
# (running as container-root) can chown its subdirectories to those users
# without any host-side permission workaround. Same fix as the restaurant/
# manager data volumes; see lib/incus.mjs in the management repo.
incus storage volume set default bookstack-data security.shifted=true

if incus info bookstack >/dev/null 2>&1; then
  echo "[deploy] Snapshotting bookstack-data before replace..."
  incus storage volume snapshot create default bookstack-data "pre-update-$(date +%Y%m%d-%H%M%S)" 2>/dev/null || true
fi

incus delete bookstack --force 2>/dev/null || true
incus launch opsavor-bookstack bookstack --profile base --profile service

sleep 3
incus config device add bookstack data disk pool=default source=bookstack-data path=/data
incus exec bookstack -- systemctl enable --now bookstack

echo "[deploy] Waiting for BookStack to become healthy (first boot runs MariaDB init + migrations — can take a couple of minutes)..."
for i in $(seq 1 48); do
  if incus exec bookstack -- curl -fsS --max-time 5 http://127.0.0.1/login >/dev/null 2>&1; then
    echo "[deploy] BookStack healthy"
    exit 0
  fi
  sleep 5
done

echo "[deploy] BookStack did not become healthy" >&2
incus exec bookstack -- journalctl -u bookstack --no-pager -n 60 2>/dev/null || true
exit 1
