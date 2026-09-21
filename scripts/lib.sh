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
