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
# The agent BINARY ships as the committed prebuilt core/vendor-common/bin/diaspore_agent (refresh it by
# cross-compiling core/agent, then re-staging). The fetched default-app APKs + generated JNI live ONLY in the
# build tree (gitignored, ~277MB) -- they are PRESERVED across re-stages so we don't re-download every build.
#
# Usage: stage-vendor.sh <repo-root> <dest = $LINEAGE_TREE/vendor/diaspore>
set -euo pipefail
REPO="${1:?usage: stage-vendor.sh <repo-root> <dest vendor/diaspore>}"
DEST="${2:?usage: stage-vendor.sh <repo-root> <dest vendor/diaspore>}"

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
cp -a "$REPO/editions/diaspore/diaspore.mk" "$DEST/diaspore.mk"

echo "[stage-vendor] assembled $DEST from core/ + editions/diaspore/"
