#!/bin/bash
# Point apex + wildcard A records at the Incus host.
# Idempotent: removes stale A records before creating new ones.
#
# Usage: DO_API_TOKEN=... providers/digitalocean/dns.sh <host-ip>
# Domain defaults to opsavor.app; override with DOMAIN=...
set -euo pipefail
: "${DO_API_TOKEN:?set DO_API_TOKEN}"
HOST_IP="${1:?usage: dns.sh <host-ip>}"
HERE="$(dirname "$0")"
DOMAIN="${DOMAIN:-opsavor.app}"

if ! [[ "$HOST_IP" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]; then
  echo "[dns] '$HOST_IP' is not an IPv4 address" >&2
  exit 1
fi

EXISTING="$($HERE/api.sh GET "/v2/domains/$DOMAIN/records?per_page=200")"
IDS="$(printf '%s' "$EXISTING" | python3 -c '
import json, sys
data = json.load(sys.stdin)
for r in data.get("domain_records", []):
    if r.get("type") == "A" and r.get("name") in ("@", "*"):
        print(r["id"])
')"

for ID in $IDS; do
  echo "[dns] removing stale A record $ID"
  $HERE/api.sh DELETE "/v2/domains/$DOMAIN/records/$ID" >/dev/null
done

for NAME in "@" "*"; do
  $HERE/api.sh POST "/v2/domains/$DOMAIN/records" \
    "{\"type\":\"A\",\"name\":\"$NAME\",\"data\":\"$HOST_IP\",\"ttl\":300}" >/dev/null
  echo "[dns] $NAME -> $HOST_IP"
done
