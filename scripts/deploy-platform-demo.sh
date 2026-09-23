#!/bin/bash
# One-off demo instances of opsavor/platform behind the edge.
#
# Usage: ./scripts/deploy-platform-demo.sh <name> <profile> [ref]
#   name:    container name + hostname (e.g. demo-bad -> demo-bad.opsavor.app)
#   profile: demo | bad | multi   (OPSAVOR_SEED)
#   ref:     image tag/ref (default: latest)
#
# Launches from the opsavor-platform image, writes the env file (seed profile +
# control token), enables the unit, health-gates, then adds an edge Caddy site
# block and reloads. Idempotent: replaces the container each run.
set -euo pipefail

NAME="${1:?usage: deploy-platform-demo.sh <name> <profile> [ref]}"
PROFILE="${2:?profile required: demo|bad|multi}"
REF="${3:-latest}"
PORT=8787

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
source "$REPO_DIR/scripts/lib.sh"
ALIAS="opsavor-platform:${REF}"

FP="$(image_fingerprint "$ALIAS")" || { echo "image $ALIAS not found — run: build-image.sh platform <ref>" >&2; exit 1; }

echo "[deploy] $NAME <- $ALIAS ($FP)  seed=$PROFILE"
incus delete "$NAME" --force 2>/dev/null || true
incus launch "$FP" "$NAME" --profile base --profile service \
  --config limits.cpu=1 --config limits.memory=1GB

echo "  waiting for network..."
for _ in $(seq 1 30); do incus exec "$NAME" -- ping -c1 -W2 8.8.8.8 >/dev/null 2>&1 && break; sleep 2; done

CTRL_TOKEN="$(openssl rand -hex 16 2>/dev/null || head -c16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
ENVF="$(mktemp)"
cat > "$ENVF" <<EOF
PORT=${PORT}
HOST=0.0.0.0
OPSAVOR_DATA=/app/.data
OPSAVOR_SEED=${PROFILE}
OPSAVOR_CONTROL_TOKEN=${CTRL_TOKEN}
EOF
incus file push "$ENVF" "$NAME/etc/default/platform"
incus exec "$NAME" -- chmod 600 /etc/default/platform
incus exec "$NAME" -- systemctl enable --now platform
rm -f "$ENVF"

IP="$(container_ip "$NAME")"
echo "  health-gating http://$IP:${PORT}/health ..."
for _ in $(seq 1 45); do curl -fsS "http://$IP:${PORT}/health" >/dev/null 2>&1 && break; sleep 2; done
if ! curl -fsS "http://$IP:${PORT}/health" >/dev/null; then
  echo "[deploy] health check FAILED for $NAME" >&2
  incus exec "$NAME" -- journalctl -u platform -n 40 --no-pager 2>&1 || true
  exit 1
fi

# Edge: one site block, wildcard DNS already points *.opsavor.app here.
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
echo "  url:     https://${NAME}.opsavor.app/app/   (health: https://${NAME}.opsavor.app/health)"
echo "  control: https://${NAME}.opsavor.app/control/v1/health  (X-Control-Token: ${CTRL_TOKEN})"
echo "  login:   owner@demo.test / opsavor   (identical on every demo)"
echo "  also:    manager@demo.test, readonly@demo.test, manager.scoped@demo.test"
