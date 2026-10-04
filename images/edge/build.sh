#!/bin/bash
# Edge image: Caddy with the DigitalOcean DNS plugin (wildcard TLS), and the plugins behind a route's
# `cache` (cache-handler) and `rate_limit` (caddy-ratelimit) options. Downloads a pre-built Caddy binary
# with them from the download API.
# Replaces do-ops/edge/Dockerfile.caddy with native LXC build.
set -euo pipefail

apt-get update -qq
apt-get install -y -qq --no-install-recommends ca-certificates curl

# Download Caddy with the plugins (pre-built, no Go toolchain needed)
curl -fsSL "https://caddyserver.com/api/download?os=linux&arch=amd64&p=github.com%2Fcaddy-dns%2Fdigitalocean&p=github.com%2Fcaddyserver%2Fcache-handler&p=github.com%2Fmholt%2Fcaddy-ratelimit" \
  -o /usr/local/bin/caddy
chmod +x /usr/local/bin/caddy

# Verify
/usr/local/bin/caddy version
modules=$(/usr/local/bin/caddy list-modules 2>&1)
for m in dns.providers.digitalocean http.handlers.cache http.handlers.rate_limit; do
  if grep -qx "$m" <<<"$modules"; then
    echo "$m: OK"
  else
    echo "$m: MISSING" >&2
    exit 1
  fi
done

# Install supporting files
mkdir -p /etc/caddy /etc/caddy/sites /var/lib/caddy /var/log/caddy

# Runs as root, no dedicated `caddy` user: binding 80/443 needs
# CAP_NET_BIND_SERVICE, and granting that to a non-root user via
# AmbientCapabilities requires matching file capabilities on the binary too
# (setcap doesn't reliably survive `incus publish`'s squashing). Root
# sidesteps that; the container itself is still unprivileged
# (security.privileged: false in profiles/base.yml), so this doesn't grant
# anything outside the container's own namespace.
#
# Caddy needs the DO_API_TOKEN env var for DNS-01. `incus config set
# environment.*` does NOT propagate to systemd services, so this uses an
# EnvironmentFile instead (see AGENTS.md gotcha #1).
cat > /etc/systemd/system/caddy.service <<'SVC'
[Unit]
Description=Caddy web server (edge)
After=network.target

[Service]
User=root
Group=root
EnvironmentFile=-/etc/default/edge
ExecStart=/usr/local/bin/caddy run --config /etc/caddy/Caddyfile --adapter caddyfile
ExecReload=/usr/local/bin/caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
SVC

systemctl enable caddy

# Placeholder env file — the actual token is injected at launch time
echo "DO_API_TOKEN=CHANGE_ME" > /etc/default/edge

# Cleanup
apt-get clean
rm -rf /var/lib/apt/lists/*
