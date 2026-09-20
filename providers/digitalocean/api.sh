#!/bin/bash
# Thin DigitalOcean API wrapper for DNS management.
# Requires DO_API_TOKEN in env.
# Usage: providers/digitalocean/api.sh GET /v2/domains/opsavor.app/records
set -euo pipefail
: "${DO_API_TOKEN:?set DO_API_TOKEN}"
METHOD="$1"; PATH_="$2"; BODY="${3:-}"
curl -sS -X "$METHOD" "https://api.digitalocean.com$PATH_" \
  -H "Authorization: Bearer $DO_API_TOKEN" \
  -H "Content-Type: application/json" \
  ${BODY:+ -d "$BODY"}
