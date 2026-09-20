#!/bin/bash
# Restaurant image: opsavor-restaurant Next.js standalone app.
# Built from a git tag. Includes all runtime dependencies.
# Replaces the restaurant/Dockerfile + compose.yml Docker pipeline.
set -euo pipefail

REF="${1:?usage: build.sh <ref>}"
REPO_URL="${RESTAURANT_REPO:-https://git.opsavor.app/opsavor/restaurant.git}"

apt-get update -qq
apt-get install -y -qq --no-install-recommends ca-certificates curl git sqlite3

# Clone at the release tag
git clone --depth 1 --branch "$REF" "$REPO_URL" /tmp/restaurant-src

# Build the Next.js standalone output
cd /tmp/restaurant-src
export NODE_OPTIONS=--max-old-space-size=2560
npm ci
npm run build

# Copy the standalone output to /app
mkdir -p /app
cp -r .next/standalone/* /app/
cp -r .next/static /app/.next/static
cp -r public /app/public
cp -r scripts /app/scripts
cp -r drizzle /app/drizzle
cp -r lib /app/lib

# Create non-root user
useradd --system --shell /usr/sbin/nologin restaurant
mkdir -p /app/.data
chown -R restaurant:restaurant /app

# Systemd service
cat > /etc/systemd/system/restaurant.service <<'SVC'
[Unit]
Description=Opsavor restaurant instance
After=network.target

[Service]
User=restaurant
Group=restaurant
WorkingDirectory=/app
ExecStart=/usr/bin/node server.js
Restart=on-failure
RestartSec=5
EnvironmentFile=-/app/.data/env

[Install]
WantedBy=multi-user.target
SVC

systemctl enable restaurant

# Cleanup build source
rm -rf /tmp/restaurant-src
apt-get clean
rm -rf /var/lib/apt/lists/*
