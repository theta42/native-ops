#!/bin/bash
# Platform image: opsavor/platform — the owner-intelligence app.
# Built from a git ref. Zero-dependency Node.js 22 (uses node:sqlite), so
# there is nothing to npm install and no native build step.
#
# The systemd unit is intentionally NOT enabled here: the deploy script writes
# /etc/default/platform (seed profile + control token) and only then enables
# and starts it, so a fresh instance seeds the right tenant instead of the
# default on first boot.
set -euo pipefail

REF="${1:?usage: build.sh <ref>}"
REPO_URL="${PLATFORM_REPO:-ssh://git@git.opsavor.work:2222/opsavor/platform.git}"
DEPLOY_KEY="${PLATFORM_DEPLOY_KEY:-/root/.ssh/gitea_opsavor_deploy}"

apt-get update -qq
apt-get install -y -qq --no-install-recommends ca-certificates curl git openssl

# Repo lives on the in-fleet Gitea (git.opsavor.work), so point SSH at the
# deploy key build-image.sh pushed in, rather than a /root/.ssh/config entry.
export GIT_SSH_COMMAND="ssh -i $DEPLOY_KEY -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"

git clone --depth 1 --branch "$REF" "$REPO_URL" /app
cd /app

# No runtime dependencies today; tolerant if one is ever added.
npm ci --production 2>/dev/null || npm install --production 2>/dev/null || true

# Non-root user + persistent data dir (bootstrap.json and data/ are gitignored,
# so neither is in the image — the DB is created and seeded on first boot).
useradd --system --shell /usr/sbin/nologin platform
mkdir -p /app/.data
chown -R platform:platform /app

cat > /etc/systemd/system/platform.service <<'SVC'
[Unit]
Description=Opsavor platform (owner intelligence)
After=network.target

[Service]
User=platform
Group=platform
WorkingDirectory=/app
Environment=PORT=8787 HOST=0.0.0.0 OPSAVOR_DATA=/app/.data
EnvironmentFile=-/etc/default/platform
ExecStart=/usr/bin/node src/server.mjs
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
SVC

# Deliberately not `systemctl enable` — see header.
apt-get clean
rm -rf /var/lib/apt/lists/*
