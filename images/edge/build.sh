#!/bin/bash
# Edge image: Caddy with DigitalOcean DNS plugin for wildcard TLS.
# Builds Caddy from source with the DNS plugin compiled in.
# Replaces do-ops/edge/Dockerfile.caddy (Docker build) with native LXC build.
set -euo pipefail

CADDY_VERSION="${CADDY_VERSION:-2.10.0}"

apt-get update -qq
apt-get install -y -qq --no-install-recommends \
  ca-certificates curl gnupg golang-go

# Build Caddy with DO DNS plugin
cd /tmp
git clone --depth 1 --branch "v$CADDY_VERSION" https://github.com/caddyserver/caddy.git
cd caddy/cmd/caddy

# Add the DigitalOcean DNS plugin
cat >> main.go <<'EOF'
import _ "github.com/caddy-dns/digitalocean"
EOF

go mod edit -require github.com/caddy-dns/digitalocean@latest
go mod tidy

CGO_ENABLED=0 go build -trimpath -o /usr/local/bin/caddy

# Verify
/usr/local/bin/caddy version
/usr/local/bin/caddy list-modules | grep -q "dns.providers.digitalocean" \
  && echo "digitalocean DNS plugin: OK" \
  || (echo "digitalocean DNS plugin: MISSING" >&2; exit 1)

# Install supporting files
mkdir -p /etc/caddy /etc/caddy/sites

# Create non-root user for Caddy
useradd --system --shell /usr/sbin/nologin --home /var/lib/caddy caddy
mkdir -p /var/lib/caddy /var/log/caddy
chown caddy:caddy /var/lib/caddy /var/log/caddy

# Systemd service for Caddy inside LXC
cat > /etc/systemd/system/caddy.service <<'SVC'
[Unit]
Description=Caddy web server (opsavor edge)
After=network.target

[Service]
User=caddy
Group=caddy
ExecStart=/usr/local/bin/caddy run --config /etc/caddy/Caddyfile --adapter caddyfile
ExecReload=/usr/local/bin/caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
Restart=on-failure
RestartSec=5
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
SVC

systemctl enable caddy

# Cleanup build toolchain (keeps image small)
apt-get purge -y golang-go
apt-get autoremove -y
apt-get clean
rm -rf /var/lib/apt/lists/* /tmp/caddy /root/go
