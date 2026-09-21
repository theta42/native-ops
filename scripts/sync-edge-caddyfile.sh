#!/bin/bash
# Push this repo's edge/Caddyfile (and the static opsavor.work landing
# page it serves) into the running edge container and reload. There was
# previously no script for this at all — the live Caddyfile was
# hand-pushed once during initial host setup and never kept in sync with
# the repo since. Run this after editing edge/Caddyfile or
# edge/opsavor-work-landing/.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"

incus file push "$REPO_DIR/edge/Caddyfile" edge/etc/caddy/Caddyfile
incus exec edge -- mkdir -p /etc/caddy/landing
# Not `-r` on the whole directory: incus file push -r pushes the source
# directory itself as a subdirectory of the target, landing the file at
# .../landing/opsavor-work-landing/index.html instead of .../landing/
# index.html where the Caddyfile's `root * /etc/caddy/landing` expects it.
incus file push "$REPO_DIR/edge/opsavor-work-landing/index.html" edge/etc/caddy/landing/index.html
incus exec edge -- caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
incus exec edge -- caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
echo "[sync] edge Caddyfile + landing page pushed and reloaded"
