#!/usr/bin/env bash
# stage-vendor.sh -- assemble the LineageOS build tree's vendor/diaspore from the restructured repo
# (DIA-20260618-09). The repo is product-shaped (core/ shared + editions/ per OS), but the LineageOS build
# expects a single vendor/diaspore tree, so we merge the pieces into it before brunch:
#
#   core/vendor-common/*               -> vendor/diaspore/         (Android.bp, bin/, etc/, sepolicy/,
#                                                                   default-permissions/, prebuilts/, media/, *.ps1)
#   core/chooser/                      -> vendor/diaspore/chooser/  (the gate app; Android.bp globs chooser/src)
#   editions/diaspore/vendor/overlay/  -> vendor/diaspore/overlay/  (LineageOS RROs)
#   editions/diaspore/diaspore.mk      -> vendor/diaspore/diaspore.mk  (inherited by device/.../lineage_FP3.mk)
#
# The agent BINARY is CROSS-COMPILED from core/agent at stage time (NOT a committed prebuilt) so it can never
# go stale against the Go source -- see the build step at the end; it needs Go on the build host. The fetched
# default-app APKs + generated JNI live ONLY in the build tree (gitignored, ~277MB) -- they are PRESERVED
# across re-stages so we don't re-download every build.
#
# Tiers (see AGENTS.md "Repository layout"): core/ = common (nowhere), editions/diaspore = OS (LineageOS),
# editions/diaspore/devices/<device> = device (fp3/...). This assembles all three into one vendor/diaspore.
# Usage: stage-vendor.sh <repo-root> <dest = $LINEAGE_TREE/vendor/diaspore> [device=fp3]
set -euo pipefail
REPO="${1:?usage: stage-vendor.sh <repo-root> <dest vendor/diaspore> [device]}"
DEST="${2:?usage: stage-vendor.sh <repo-root> <dest vendor/diaspore> [device]}"
DEV="${3:-fp3}"

mkdir -p "$DEST"
# Shared core. --delete keeps DEST in sync with the repo, but PROTECT the fetched prebuilts (APKs, extracted
# JNI, generated fragment) -- they're not in the repo and are expensive to re-fetch.
rsync -a --delete \
  --exclude 'prebuilts/*.apk' \
  --exclude 'prebuilts/jni/' \
  --exclude 'prebuilts/default-apps-jni.mk' \
  --exclude '/chooser' --exclude '/overlay' --exclude '/diaspore.mk' \
  "$REPO/core/vendor-common/" "$DEST/"
# The chooser app (kept as a top-level core component; Android.bp expects it at vendor/diaspore/chooser).
rsync -a --delete "$REPO/core/chooser/" "$DEST/chooser/"
# Edition (Fairphone + LineageOS) pieces.
mkdir -p "$DEST/overlay"
rsync -a --delete "$REPO/editions/diaspore/vendor/overlay/" "$DEST/overlay/"
# Device tier: overlay the per-device delta (the FP3 Trebuchet home layout) on TOP of the OS-common overlay,
# so vendor/diaspore ends up identical to the old single-device layout. A new LineageOS device adds only
# editions/diaspore/devices/<device>/vendor without touching the OS-common edition root.
if [ -d "$REPO/editions/diaspore/devices/$DEV/vendor" ]; then
  rsync -a "$REPO/editions/diaspore/devices/$DEV/vendor/" "$DEST/"
  echo "[stage-vendor] overlaid device '$DEV' vendor delta"
else
  echo "[stage-vendor] WARNING: no device delta at editions/diaspore/devices/$DEV/vendor" >&2
fi
cp -a "$REPO/editions/diaspore/diaspore.mk" "$DEST/diaspore.mk"

# Build the agent FROM SOURCE into the staged tree (it is NOT a committed prebuilt, so it can't drift from
# core/agent). Static arm64 / CGO off -- the device target. Fails LOUDLY if Go is missing or the build
# breaks, rather than silently shipping a stale binary (the old committed-prebuilt footgun, DIA-20260629-26).
GO_BIN="$(command -v go || true)"
[ -z "$GO_BIN" ] && [ -x /usr/local/go/bin/go ] && GO_BIN=/usr/local/go/bin/go
if [ -z "$GO_BIN" ]; then
  echo "[stage-vendor] ERROR: 'go' not found -- needed to build vendor/diaspore/bin/nowhere_agent from core/agent" >&2
  exit 1
fi
mkdir -p "$DEST/bin"
( cd "$REPO/core/agent" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 "$GO_BIN" build -o "$DEST/bin/nowhere_agent" . )
chmod 755 "$DEST/bin/nowhere_agent"
echo "[stage-vendor] built agent ($("$GO_BIN" version | awk '{print $3}')) -> $(file "$DEST/bin/nowhere_agent" | grep -oE 'BuildID\[sha1\]=[0-9a-f]+' || echo ok)"

echo "[stage-vendor] assembled $DEST from core/ + editions/diaspore/"
