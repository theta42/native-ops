#!/bin/bash
# Deploy the opsavor.ai home page: build an image from the opsavor.ai repo,
# replace the running `home` container, health-gate. Called by the Gitea
# Actions runner or manually.
#
# Usage: ./scripts/deploy-home.sh [ref]
#   ref: git tag/branch in the opsavor/opsavor.ai repo. Defaults to "main".
#
# The site is stateless (no data volume): a deploy is build -> delete ->
# launch -> health-gate. After changing edge/Caddyfile, run
# ./scripts/sync-edge-caddyfile.sh too.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
source "$REPO_DIR/scripts/lib.sh"

REF="${1:-main}"

echo "[deploy] Building opsavor-home:$REF"
"$REPO_DIR/scripts/build-image.sh" home "$REF"
FP=$(image_fingerprint "opsavor-home:$REF")

# Replace the container. There is no persistent volume to preserve.
incus delete home --force 2>/dev/null || true

incus launch "$FP" home --profile base --profile service

sleep 3

# Env for systemd (EnvironmentFile, not `incus config set environment.*` —
# see AGENTS.md gotcha #1: the latter never reaches a systemd-managed
# process). Defaults already live in the unit; this file is where a future
# backend's secrets/values would be written from /root/.env.
incus exec home -- bash -c "cat > /etc/default/home <<EOF
PORT=3000
HOST=0.0.0.0
HOME_ROOT=/app
EOF
chown home:home /etc/default/home
chmod 600 /etc/default/home"

incus restart home

echo "[deploy] Waiting for home to become healthy..."
for i in $(seq 1 24); do
  if incus exec home -- curl -fsS --max-time 5 http://127.0.0.1:3000/health >/dev/null 2>&1; then
    echo "[deploy] Home healthy @ $REF"
    exit 0
  fi
  sleep 5
done

echo "[deploy] Home did not become healthy" >&2
incus exec home -- journalctl -u home --no-pager -n 20 2>/dev/null || true
exit 1
