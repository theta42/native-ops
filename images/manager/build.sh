#!/bin/bash
# Manager image: opsavor-management fleet orchestrator.
# Built from a git tag. Zero-dep Node.js app.
set -euo pipefail

REF="${1:?usage: build.sh <ref>}"
REPO_URL="${MANAGER_REPO:-https://git.opsavor.app/opsavor/management.git}"

apt-get update -qq
apt-get install -y -qq --no-install-recommends ca-certificates curl git

# Clone at the release tag
git clone --depth 1 --branch "$REF" "$REPO_URL" /app

# Install production dependencies (only what's needed at runtime)
cd /app
npm ci --production 2>/dev/null || npm install --production

# Create non-root user
useradd --system --shell /usr/sbin/nologin manager
chown -R manager:manager /app

# Incus API access: the manager needs the incus CLI + unix socket access
apt-get install -y -qq incus-client

# Systemd service
cat > /etc/systemd/system/manager.service <<'SVC'
[Unit]
Description=Opsavor fleet manager
After=network.target

[Service]
User=manager
Group=manager
WorkingDirectory=/app
ExecStart=/usr/bin/node server.mjs
Restart=on-failure
RestartSec=5
EnvironmentFile=-/app/.data/env

[Install]
WantedBy=multi-user.target
SVC

systemctl enable manager

# Cleanup
apt-get clean
rm -rf /var/lib/apt/lists/*
