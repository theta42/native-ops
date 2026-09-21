#!/bin/bash
# Deploy Gitea: build the image, create/attach its persistent data volume,
# launch or replace the container, health-gate.
#
# Usage: ./scripts/deploy-gitea.sh
#
# The public URL and all generated secrets (SECRET_KEY, INTERNAL_TOKEN,
# JWT_SECRET, the local admin password) live in /data/gitea-secrets.env on
# the volume, written once on first boot — see gitea-entrypoint.sh. To
# enable Google OAuth2 login, or change the URL, edit that file and
# `incus restart gitea` (see README "Google Workspace SSO for Gitea").
#
# gitea.service is NOT enabled in the image: this script starts it
# explicitly, AFTER attaching the data volume — see native-ops AGENTS.md
# for the launch race this avoids (incus launch boots the container
# before the persistent volume is attached).
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
source "$REPO_DIR/scripts/lib.sh"

echo "[deploy] Building opsavor-gitea..."
"$REPO_DIR/scripts/build-image.sh" gitea

incus storage volume create default gitea-data 2>/dev/null || true
# Same fix as bookstack/restaurant/manager: lets the container's own uid
# mapping (the `git` user) apply to this volume, so the entrypoint
# (running as container-root before it drops to git) can chown it without
# any host-side permission workaround.
incus storage volume set default gitea-data security.shifted=true

if incus info gitea >/dev/null 2>&1; then
  echo "[deploy] Snapshotting gitea-data before replace..."
  incus storage volume snapshot create default gitea-data "pre-update-$(date +%Y%m%d-%H%M%S)" 2>/dev/null || true
fi

incus delete gitea --force 2>/dev/null || true
incus launch opsavor-gitea gitea --profile base --profile service

sleep 3
incus config device add gitea data disk pool=default source=gitea-data path=/data
# Public git-over-SSH: a deliberate exception to "no public listener except
# edge" (see README's Design principles / Firewall) — git-over-ssh is a raw
# TCP protocol Caddy can't reverse-proxy the way it does gitea's HTTP
# traffic, so this is its own proxy device straight from the host's public
# interface into the container, parallel to (not instead of) edge's 80/443.
incus config device add gitea ssh-git proxy listen=tcp:0.0.0.0:2222 connect=tcp:127.0.0.1:2222 2>/dev/null || true
incus exec gitea -- systemctl enable --now gitea

echo "[deploy] Waiting for Gitea to become healthy..."
for i in $(seq 1 24); do
  if incus exec gitea -- curl -fsS --max-time 5 http://127.0.0.1:3000/api/healthz >/dev/null 2>&1; then
    echo "[deploy] Gitea healthy"
    exit 0
  fi
  sleep 5
done

echo "[deploy] Gitea did not become healthy" >&2
incus exec gitea -- journalctl -u gitea --no-pager -n 60 2>/dev/null || true
exit 1
