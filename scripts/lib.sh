#!/bin/bash
# Shared helpers for the scripts in this directory. Source, don't execute.

# Look up an image fingerprint by exact alias name.
#
# `incus image alias list`'s table output is column-aligned with │
# separators, and awk's default whitespace field-splitting counts each │ as
# its own field — `{print $2}` on a matched row prints the ALIAS back
# (field 2), not the FINGERPRINT (field 4). Every script that shelled out to
# that awk one-liner therefore fed the colon-bearing alias (e.g.
# "opsavor-manager:latest") straight into `incus launch`, which is exactly
# the string gotcha #3 warns is misparsed as a remote name. Parse the JSON
# instead so this can't silently drift again with a column-width change.
image_fingerprint() {
  local alias="$1"
  incus image alias list --format json | python3 -c '
import json, sys
alias = sys.argv[1]
for a in json.load(sys.stdin):
    if a.get("name") == alias:
        print(a["target"])
        sys.exit(0)
sys.exit(1)
' "$alias"
}

# Look up a running container's bridge IPv4 address.
#
# `.incus` mDNS-style names only resolve from INSIDE other containers on
# the bridge (Incus serves that DNS on incusbr0 itself) — the host is not a
# client of it, so `curl http://manager.incus:3001/...` from a script
# running directly on the host (as the CI runner now does) fails with
# "Could not resolve host". The documented 10.0.100.x addresses per
# container are also not load-bearing: real containers come up on
# whatever the bridge's DHCP hands out, which drifts across replaces. Ask
# Incus directly instead of assuming either.
container_ip() {
  local name="$1"
  incus list "$name" --format json | python3 -c '
import json, sys
data = json.load(sys.stdin)
if not data:
    sys.exit(1)
for addr in data[0]["state"]["network"]["eth0"]["addresses"]:
    if addr["family"] == "inet":
        print(addr["address"])
        sys.exit(0)
sys.exit(1)
'
}
