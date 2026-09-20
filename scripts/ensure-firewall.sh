#!/bin/bash
# Host firewall for the Incus node.
# Only public ports: 80, 443, 22. Everything internal stays on incusbr0.
# Idempotent.
#
# Usage: ./scripts/ensure-firewall.sh [admin-cidr]
# Default admin CIDR blocks all SSH except IPv6 localhost — pass your real
# admin IP to allow remote access.
set -euo pipefail

ADMIN_CIDR="${1:-127.0.0.1/32}"

if [ "$ADMIN_CIDR" = "127.0.0.1/32" ]; then
  echo "[firewall] WARNING: no admin CIDR given, SSH will be blocked." >&2
  echo "[firewall] Re-run with your admin IP as arg, e.g.: ./scripts/ensure-firewall.sh 203.0.113.1/32" >&2
fi

echo "[firewall] Configuring host firewall..."

ufw default deny incoming
ufw default allow outgoing

# Public services
ufw allow 80/tcp comment "HTTP"
ufw allow 443/tcp comment "HTTPS"

# SSH (admin only)
if [ "$ADMIN_CIDR" != "127.0.0.1/32" ]; then
  ufw allow from "$ADMIN_CIDR" to any port 22 proto tcp comment "SSH admin"
fi

# Incus API (multi-host only — uncomment when adding workers)
# ufw allow from <worker-node-cidr> to any port 8443 proto tcp comment "Incus API"

ufw --force enable

echo "[firewall] Done."
ufw status
