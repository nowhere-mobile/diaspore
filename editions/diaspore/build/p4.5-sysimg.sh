#!/bin/bash
# P4.5 FAST PATH: rebuild ONLY the system image (+ vbmeta) so the Diaspore additions
# (agent, init .rc, diaspore.conf, boot animation) get baked in, WITHOUT a full brunch.
# The vanilla LineageOS compile is already complete and ccache-warm, so this is
# essentially stage + mkfs, not a recompile. NOTE: do NOT use `set -u`/`set -e` —
# LineageOS envsetup.sh/breakfast are not safe under them.
cd /mnt/build/lineage || { echo "NO_TREE"; exit 1; }
export CCACHE_DIR=/mnt/build/ccache
export USE_CCACHE=1
source build/envsetup.sh
breakfast FP3
echo "=== START $(date -u) ==="
m systemimage vbmetaimage
ec=$?
echo "=== SYSIMG_EXIT=$ec $(date -u) ==="
ls -la out/target/product/FP3/system.img out/target/product/FP3/vbmeta.img 2>/dev/null
echo "--- diaspore artifacts staged into the system partition? ---"
find out/target/product/FP3/system \( -name diaspore_agent -o -name bootanimation.zip -o -name 'diaspore*.rc' -o -name diaspore.conf \) 2>/dev/null
