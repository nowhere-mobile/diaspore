#!/usr/bin/env bash
# Phase 4 / P4.1: repo sync the LineageOS lineage-22.2 base tree onto the build disk.
set -uxo pipefail
SRC=/mnt/build/lineage
export PATH="$HOME/bin:$PATH"
cd "$SRC"
echo "=== repo sync start $(date) (16 jobs, shallow/current-branch) ==="
repo sync -c -j16 --force-sync --no-clone-bundle --no-tags 2>&1 | tail -60
RC=${PIPESTATUS[0]}
echo "=== repo sync rc=$RC  $(date) ==="
echo "=== size + free ==="; du -sh "$SRC" 2>/dev/null | tail -1; df -h /mnt/build | tail -1
echo "=== common-tree sanity (device/vendor repos come later via breakfast FP3) ==="
for d in build/make bionic frameworks/base system/core vendor/lineage; do
  [ -d "$SRC/$d" ] && echo "ok  $d" || echo "MISSING  $d"
done
echo "P4_1_SYNC_DONE rc=$RC"
