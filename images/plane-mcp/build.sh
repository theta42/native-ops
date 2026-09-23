#!/bin/bash
# plane-mcp image: Plane's official MCP server (github.com/makeplane/plane-mcp-server).
#
# Python/FastMCP. Runs the HTTP transport (uvicorn on :8211), which serves:
#   /http/api-key/mcp  <- header auth: Authorization: Bearer <Plane PAT> + x-workspace-slug
#   /http/mcp          <- OAuth transport (needs PLANE_OAUTH_PROVIDER_*)
#   /sse               <- deprecated SSE
#
# The header-auth transport validates the caller's token against the Plane API
# (PLANE_INTERNAL_BASE_URL) per request, so the service holds no Plane secret
# itself — agents bring their own PAT.
set -euo pipefail

apt-get update -qq
apt-get install -y -qq --no-install-recommends python3 python3-venv ca-certificates

python3 -m venv /opt/plane-mcp
/opt/plane-mcp/bin/pip install --quiet --upgrade pip
/opt/plane-mcp/bin/pip install --quiet plane-mcp-server

useradd --system --shell /usr/sbin/nologin plane-mcp

cat > /etc/systemd/system/plane-mcp.service <<'SVC'
[Unit]
Description=Plane MCP Server (HTTP)
After=network.target

[Service]
User=plane-mcp
Group=plane-mcp
EnvironmentFile=-/etc/default/plane-mcp
ExecStart=/opt/plane-mcp/bin/plane-mcp-server http
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
SVC

# Left disabled — deploy-plane-mcp.sh writes the env before starting.
apt-get clean
rm -rf /var/lib/apt/lists/*
