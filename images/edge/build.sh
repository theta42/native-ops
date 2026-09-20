#!/bin/bash
# Edge image: Caddy with DigitalOcean DNS plugin for wildcard TLS.
# Downloads pre-built Caddy binary with the DO DNS plugin from the download API.
# Replaces do-ops/edge/Dockerfile.caddy with native LXC build.
set -euo pipefail

apt-get update -qq
apt-get install -y -qq --no-install-recommends ca-certificates curl

# Download Caddy with DO DNS plugin (pre-built, no Go toolchain needed)
curl -fsSL "https://caddyserver.com/api/download?os=linux&arch=amd64&p=github.com%2Fcaddy-dns%2Fdigitalocean" \
  -o /usr/local/bin/caddy
chmod +x /usr/local/bin/caddy

# Verify
/usr/local/bin/caddy version
/usr/local/bin/caddy list-modules 2>&1 | grep -q "dns.providers.digitalocean" \
  && echo "digitalocean DNS plugin: OK" \
  || (echo "digitalocean DNS plugin: MISSING" >&2; exit 1)

# Install supporting files
mkdir -p /etc/caddy /etc/caddy/sites /var/lib/caddy /var/log/caddy

# Create user for caddy
useradd --system --shell /usr/sbin/nologin --home /var/lib/caddy caddy 2>/dev/null || true
chown caddy:caddy /var/lib/caddy /var/log/caddy

# Systemd service — Caddy needs the DO_API_TOKEN env var for DNS-01.
# incus config set environment.* does NOT propagate to systemd services,
# so we use an EnvironmentFile.
cat > /etc/systemd/system/caddy.service <<'SVC'
[Unit]
Description=Caddy web server (opsavor edge)
After=network.target

[Service]
User=caddy
Group=caddy
EnvironmentFile=-/etc/default/edge
ExecStart=/usr/local/bin/caddy run --config /etc/caddy/Caddyfile --adapter caddyfile
ExecReload=/usr/local/bin/caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
Restart=on-failure
RestartSec=5
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
SVC

systemctl enable caddy

# Placeholder env file — the actual token is injected at launch time
echo "DO_API_TOKEN=CHANGE_ME" > /etc/default/edge

# Cleanup
apt-get clean
rm -rf /var/lib/apt/lists/*
