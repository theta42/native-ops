#!/bin/bash
# Deploy the Plane MCP server behind the edge.
#
# Usage: ./scripts/deploy-plane-mcp.sh [ref]
# Serves https://plane-mcp.opsavor.app/http/api-key/mcp  (header auth).
#
# No server-side Plane credentials: the header transport validates each caller's
# own PAT against PLANE_INTERNAL_BASE_URL per request.
set -euo pipefail

NAME=plane-mcp
PORT=8211
REF="${1:-latest}"
PLANE_API="${PLANE_API_URL:-https://plane.opsavor.app}"

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
source "$REPO_DIR/scripts/lib.sh"
ALIAS="opsavor-plane-mcp:${REF}"
FP="$(image_fingerprint "$ALIAS")" || { echo "image $ALIAS not found — run: build-image.sh plane-mcp <ref>" >&2; exit 1; }

echo "[deploy] $NAME <- $ALIAS ($FP)"
incus delete "$NAME" --force 2>/dev/null || true
incus launch "$FP" "$NAME" --profile base --profile service \
  --config limits.cpu=1 --config limits.memory=1GB

echo "  waiting for network..."
for _ in $(seq 1 30); do incus exec "$NAME" -- ping -c1 -W2 8.8.8.8 >/dev/null 2>&1 && break; sleep 2; done

ENVF="$(mktemp)"
cat > "$ENVF" <<EOF
PLANE_INTERNAL_BASE_URL=${PLANE_API}
PLANE_BASE_URL=${PLANE_API}
LOG_PAYLOADS=false
LOG_USER_INFO=false
# HTTP mode always builds the OAuth transport too, which refuses to construct
# without a client id/secret. We serve the header-auth transport
# (/http/api-key/mcp); these placeholders only satisfy construction.
PLANE_OAUTH_PROVIDER_CLIENT_ID=unused
PLANE_OAUTH_PROVIDER_CLIENT_SECRET=unused
PLANE_OAUTH_PROVIDER_BASE_URL=https://${NAME}.opsavor.app
EOF
incus file push "$ENVF" "$NAME/etc/default/plane-mcp"
incus exec "$NAME" -- chmod 600 /etc/default/plane-mcp
incus exec "$NAME" -- systemctl enable --now plane-mcp
rm -f "$ENVF"

IP="$(container_ip "$NAME")"
echo "  health-gating http://$IP:${PORT}/http/api-key/mcp ..."
for _ in $(seq 1 45); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "http://$IP:${PORT}/http/api-key/mcp" || true)"
  [ -n "$code" ] && [ "$code" != "000" ] && break
  sleep 2
done
code="$(curl -s -o /dev/null -w '%{http_code}' "http://$IP:${PORT}/http/api-key/mcp" || true)"
if [ -z "$code" ] || [ "$code" = "000" ]; then
  echo "[deploy] $NAME not responding" >&2
  incus exec "$NAME" -- journalctl -u plane-mcp -n 40 --no-pager 2>&1 || true
  exit 1
fi

SITE="$(mktemp)"
cat > "$SITE" <<EOF
${NAME}.opsavor.app {
	tls {
		dns digitalocean {env.DO_API_TOKEN}
	}
	import strip-forged-identity
	reverse_proxy ${NAME}:${PORT}
}
EOF
incus exec edge -- mkdir -p /etc/caddy/sites
incus file push "$SITE" "edge/etc/caddy/sites/${NAME}.caddy"
rm -f "$SITE"
incus exec edge -- caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
incus exec edge -- caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile

echo "[deploy] $NAME ready (first probe: HTTP $code)"
echo "  url:  https://${NAME}.opsavor.app/http/api-key/mcp"
echo "  env:  PLANE_INTERNAL_BASE_URL=${PLANE_API}"
echo "  client config:"
echo "    { \"mcpServers\": { \"plane\": { \"url\": \"https://${NAME}.opsavor.app/http/api-key/mcp\","
echo "        \"headers\": { \"Authorization\": \"Bearer <PLANE_PAT>\", \"X-Workspace-slug\": \"<workspace>\" } } } }"
