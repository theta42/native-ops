#!/bin/bash
# Home image: the opsavor.ai marketing site.
# Built from a git tag. Zero-dep Node.js app (static server; a backend will
# be added to server.mjs later — see the opsavor.ai repo's README).
set -euo pipefail

REF="${1:?usage: build.sh <ref>}"
REPO_URL="${HOME_REPO:-ssh://git@git.opsavor.work:2222/opsavor/opsavor.ai.git}"
DEPLOY_KEY="${HOME_DEPLOY_KEY:-/root/.ssh/gitea_opsavor_deploy}"

apt-get update -qq
apt-get install -y -qq --no-install-recommends ca-certificates curl git

# The repo lives on the in-fleet Gitea (git.opsavor.work), not git.theta42.com
# like manager — so build-image.sh pushes a separate deploy key
# (gitea_opsavor_deploy) and this build points SSH at it explicitly rather
# than depending on a /root/.ssh/config entry.
export GIT_SSH_COMMAND="ssh -i $DEPLOY_KEY -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"

# Clone at the release tag
git clone --depth 1 --branch "$REF" "$REPO_URL" /app

# Install production dependencies. There are none today (server.mjs is
# zero-dependency), so npm may have nothing to do — kept tolerant so it stays
# correct once a backend adds a package.json.
cd /app
npm ci --production 2>/dev/null || npm install --production 2>/dev/null || true

# Create non-root user
useradd --system --shell /usr/sbin/nologin home
mkdir -p /app/.data
chown -R home:home /app

# Systemd service — env vars must come from a file or Environment= lines,
# NOT from incus config set (which only affects exec sessions; see AGENTS.md
# gotcha #1).
cat > /etc/systemd/system/home.service <<'SVC'
[Unit]
Description=Opsavor home page (opsavor.ai)
After=network.target

[Service]
User=home
Group=home
WorkingDirectory=/app
Environment=PORT=3000 HOST=0.0.0.0 HOME_ROOT=/app
EnvironmentFile=-/etc/default/home
ExecStart=/usr/bin/node server.mjs
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
SVC

systemctl enable home

# Cleanup
apt-get clean
rm -rf /var/lib/apt/lists/*
