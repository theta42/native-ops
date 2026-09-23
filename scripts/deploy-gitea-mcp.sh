#!/bin/bash
# Deploy the Gitea MCP server behind the edge.
#
# Usage: ./scripts/deploy-gitea-mcp.sh [ref]
# Serves https://git-mcp.opsavor.app/mcp  (per-request token passthrough).
set -euo pipefail

NAME=git-mcp
PORT=8080
REF="${1:-latest}"
GITEA_HOST="${GITEA_HOST:-https://git.opsavor.app}"

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
source "$REPO_DIR/scripts/lib.sh"
ALIAS="opsavor-gitea-mcp:${REF}"
FP="$(image_fingerprint "$ALIAS")" || { echo "image $ALIAS not found — run: build-image.sh gitea-mcp <ref>" >&2; exit 1; }

echo "[deploy] $NAME <- $ALIAS ($FP)"
incus delete "$NAME" --force 2>/dev/null || true
incus launch "$FP" "$NAME" --profile base --profile service \
  --config limits.cpu=1 --config limits.memory=1GB

echo "  waiting for network..."
for _ in $(seq 1 30); do incus exec "$NAME" -- ping -c1 -W2 8.8.8.8 >/dev/null 2>&1 && break; sleep 2; done

ENVF="$(mktemp)"
cat > "$ENVF" <<EOF
GITEA_HOST=${GITEA_HOST}
EOF
incus file push "$ENVF" "$NAME/etc/default/gitea-mcp"
incus exec "$NAME" -- chmod 600 /etc/default/gitea-mcp
incus exec "$NAME" -- systemctl enable --now gitea-mcp
rm -f "$ENVF"

IP="$(container_ip "$NAME")"
echo "  health-gating http://$IP:${PORT}/healthz ..."
for _ in $(seq 1 45); do
  curl -fsS "http://$IP:${PORT}/healthz" >/dev/null 2>&1 && break
  sleep 2
done
if ! curl -fsS "http://$IP:${PORT}/healthz" >/dev/null; then
  echo "[deploy] $NAME health check FAILED" >&2
  incus exec "$NAME" -- journalctl -u gitea-mcp -n 40 --no-pager 2>&1 || true
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

echo "[deploy] $NAME ready"
echo "  url:  https://${NAME}.opsavor.app/mcp   (health: https://${NAME}.opsavor.app/healthz)"
echo "  host: ${GITEA_HOST}"
echo "  client config:"
echo "    { \"mcpServers\": { \"gitea\": { \"url\": \"https://${NAME}.opsavor.app/mcp\","
echo "        \"headers\": { \"Authorization\": \"Bearer <GITEA_TOKEN>\" } } } }"
