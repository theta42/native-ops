#!/bin/bash
# Daily snapshot of every custom Incus storage volume — the "Layer 2" of
# the three-layer backup model in README.md ("Storage" section). Local and
# rsync-based: the `default` pool is the `dir` driver (ZFS's DKMS build
# fails on DO's kernel — see AGENTS.md/the day-1 log), so there is no `zfs
# send` off-site equivalent here. This protects against a bad deploy or
# operator error, not against losing the disk — that still needs the DO
# droplet backup (manual, DO console) or an actual off-site copy of this
# pool, neither of which this script does.
#
# Usage: ./scripts/snapshot-all.sh
# Installed as a daily systemd timer — see snapshot-all.service/.timer in
# this same directory; `systemctl enable --now snapshot-all.timer` once,
# on the Incus host.
set -euo pipefail

POOL="default"
KEEP="${SNAPSHOT_KEEP:-14}"
STAMP="$(date -u +%Y%m%d-%H%M%S)"
PREFIX="daily-"

custom_volumes() {
  incus storage volume list "$POOL" --format json | python3 -c '
import json, sys
for v in json.load(sys.stdin):
    if v.get("type") == "custom":
        print(v["name"])
'
}

# Snapshot names this script prunes ("$PREFIX*"), oldest first, one per
# line. Never touches a deploy script's own "pre-update-*" snapshots.
own_snapshots_oldest_first() {
  local vol="$1"
  incus storage volume snapshot list "$POOL" "$vol" --format json | python3 -c '
import json, sys
prefix = sys.argv[1]
snaps = json.load(sys.stdin)
named = [(s["created_at"], s["name"].split("/", 1)[1]) for s in snaps if s["name"].split("/", 1)[1].startswith(prefix)]
named.sort()
for _, name in named:
    print(name)
' "$PREFIX"
}

failures=0
for vol in $(custom_volumes); do
  echo "[snapshot] $vol -> ${PREFIX}${STAMP}"
  if ! incus storage volume snapshot create "$POOL" "$vol" "${PREFIX}${STAMP}"; then
    echo "[snapshot] FAILED: $vol" >&2
    failures=$((failures + 1))
    continue
  fi

  mapfile -t existing < <(own_snapshots_oldest_first "$vol")
  extra=$(( ${#existing[@]} - KEEP ))
  if [ "$extra" -gt 0 ]; then
    for name in "${existing[@]:0:$extra}"; do
      echo "  pruning $vol/$name (keeping newest $KEEP)"
      incus storage volume snapshot delete "$POOL" "$vol" "$name" || true
    done
  fi
done

echo "[snapshot] done: $(custom_volumes | wc -l) volumes, $failures failure(s)"
[ "$failures" -eq 0 ]
