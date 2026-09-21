#!/bin/bash
# Restaurant image: opsavor-restaurant Next.js standalone app.
# Built from a git tag. Includes all runtime dependencies.
# Replaces the restaurant/Dockerfile + compose.yml Docker pipeline.
set -euo pipefail

REF="${1:?usage: build.sh <ref>}"
REPO_URL="${RESTAURANT_REPO:-ssh://gitea@git.theta42.com:2222/opsavor/restaurant.git}"

apt-get update -qq
# build-essential + python3: better-sqlite3 falls back to compiling its
# native binding from source when no prebuilt matches this glibc/Node
# combo. Purged again below once the build is done (mirrors the Dockerfile's
# multi-stage image, which never shipped the build stage's apt packages).
apt-get install -y -qq --no-install-recommends \
  ca-certificates curl git sqlite3 build-essential python3

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

# Systemd service. ExecStart runs the same entrypoint the old Docker image
# used (staged restore -> migrate -> optional owner seed -> server.js),
# NOT `node server.js` directly — a plain ExecStart would boot the app
# straight past pending drizzle migrations and owner seeding on every first
# launch after an onboard or an image update.
cat > /etc/systemd/system/restaurant.service <<'SVC'
[Unit]
Description=Opsavor restaurant instance
After=network.target

[Service]
User=restaurant
Group=restaurant
WorkingDirectory=/app
Environment=NODE_ENV=production PORT=3000 HOSTNAME=0.0.0.0 DATA_DIR=/app/.data
EnvironmentFile=-/app/.data/env
ExecStart=/bin/bash scripts/docker-entrypoint.sh
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
SVC

systemctl enable restaurant

# Cleanup build source and build-only dependencies
rm -rf /tmp/restaurant-src
apt-get purge -y -qq build-essential python3
apt-get autoremove -y -qq
apt-get clean
rm -rf /var/lib/apt/lists/*
