#!/bin/bash
# gitea-mcp image: the official Gitea MCP server (gitea.com/gitea/gitea-mcp).
#
# Runs the HTTP transport (port 8080, /healthz, /mcp). Supports per-request
# credential passthrough: `Authorization: Bearer <Gitea token>` (or `token <…>`),
# so the service needs no Gitea secret of its own — agents bring their own token.
set -euo pipefail

GITEA_MCP_VERSION="${GITEA_MCP_VERSION:-v1.7.0}"
ASSET="gitea-mcp_Linux_x86_64.tar.gz"
URL="https://gitea.com/gitea/gitea-mcp/releases/download/${GITEA_MCP_VERSION}/${ASSET}"

apt-get update -qq
apt-get install -y -qq --no-install-recommends ca-certificates curl tar

curl -fsSL "$URL" -o /tmp/gitea-mcp.tar.gz
tar -xzf /tmp/gitea-mcp.tar.gz -C /tmp
install -m 0755 /tmp/gitea-mcp /usr/local/bin/gitea-mcp
rm -f /tmp/gitea-mcp.tar.gz /tmp/gitea-mcp

useradd --system --shell /usr/sbin/nologin gitea-mcp

cat > /etc/systemd/system/gitea-mcp.service <<'SVC'
[Unit]
Description=Gitea MCP Server (HTTP)
After=network.target

[Service]
User=gitea-mcp
Group=gitea-mcp
EnvironmentFile=-/etc/default/gitea-mcp
ExecStart=/usr/local/bin/gitea-mcp -t http -p 8080
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
SVC

# Left disabled — deploy-gitea-mcp.sh writes the env before starting.
apt-get clean
rm -rf /var/lib/apt/lists/*
