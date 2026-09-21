#!/bin/bash
# Push this repo's edge/Caddyfile into the running edge container and
# reload. There was previously no script for this at all — the live
# Caddyfile was hand-pushed once during initial host setup and never kept
# in sync with the repo since. Run this after editing edge/Caddyfile.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"

incus file push "$REPO_DIR/edge/Caddyfile" edge/etc/caddy/Caddyfile
incus exec edge -- caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
incus exec edge -- caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
echo "[sync] edge Caddyfile pushed and reloaded"
