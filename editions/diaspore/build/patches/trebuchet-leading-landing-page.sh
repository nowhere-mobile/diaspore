#!/bin/bash
# DIA-20260618-01 -- Trebuchet (Launcher3) out-of-repo patch: keep a clean empty LANDING page.
#
# WHY: the Diaspore default home (vendor/diaspore/overlay/.../default_workspace_5x*.xml) puts the 5 primary
# apps in the hotseat (dock) and the rest on screen 1 (page 2), leaving screen 0 (the landing) empty so you
# land on wallpaper + dock, not a wall of icons. But Launcher3 only INSERTS that empty leading page (via
# Workspace.bindAndInitFirstWorkspaceScreen) when QSB_ON_FIRST_SCREEN is true; otherwise the page-2 apps
# collapse onto the landing. Trebuchet ships QSB_ON_FIRST_SCREEN=false, so flip it true.
#
# The QSB would normally draw a (dead, no-GApps) Google search bar on that page -- we suppress it IN-REPO by
# overlaying res/layout/search_container_workspace.xml with an empty 0-height QsbContainerView (see
# vendor/diaspore/overlay/packages/apps/Trebuchet/res/layout/search_container_workspace.xml). So this script
# only needs to flip the build flag.
#
# This edits the LineageOS tree (packages/apps/Trebuchet), not the diaspore repo -- same class as the
# device-tree AVB BoardConfig edits; run once on the build tree before `m`. Idempotent.
#
# Usage: trebuchet-leading-landing-page.sh [LINEAGE_TREE]   (default: /mnt/build/lineage)
set -euo pipefail
TREE="${1:-/mnt/build/lineage}"
TB="$TREE/packages/apps/Trebuchet"
[ -d "$TB" ] || { echo "not found: $TB" >&2; exit 1; }

changed=0
for bc in src_build_config/com/android/launcher3/BuildConfig.java \
          go/quickstep/src/com/android/launcher3/BuildConfig.java; do
  f="$TB/$bc"
  [ -f "$f" ] || continue
  if grep -q "QSB_ON_FIRST_SCREEN = false;" "$f"; then
    cp -n "$f" "$f.diaspore-orig" 2>/dev/null || true
    sed -i "s/QSB_ON_FIRST_SCREEN = false;/QSB_ON_FIRST_SCREEN = true;/" "$f"
    changed=1
  fi
  echo "$bc -> $(grep -m1 QSB_ON_FIRST_SCREEN "$f")"
done
[ "$changed" = 1 ] && echo "patched QSB_ON_FIRST_SCREEN=true" || echo "already true (no change)"
