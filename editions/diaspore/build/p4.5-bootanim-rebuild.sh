#!/bin/bash
# Round-3 fix: bake the STORED bootanimation so it actually plays.
# Soong does NOT track TARGET_BOOTANIMATION (vendor/diaspore/media/bootanimation.zip) as an input of
# the lineage_bootanimation genrule — the path is a soong_config string baked into the genrule's `cp`
# command — so re-zipping the source left soong's CACHED *deflated* output in the image (bootanimation
# mmaps frames, which fails on deflated entries => blank boot screen). Force a clean re-copy by removing
# the cached genrule + prebuilt_media + staged outputs and bumping the genrule's tracked inputs, then
# repackage system + vbmeta. No .bp/.mk changed, so breakfast's analysis is a no-op (fast).
cd /mnt/build/lineage || exit 1
rm -f out/soong/.intermediates/vendor/lineage/bootanimation/gen-bootanimation.zip/android_common/bootanimation.zip
rm -f out/soong/.intermediates/vendor/lineage/bootanimation/bootanimation.zip/android_common/bootanimation.zip
rm -f out/target/product/FP3/system/product/media/bootanimation.zip
touch vendor/lineage/bootanimation/desc.txt vendor/lineage/bootanimation/gen-bootanimation.sh
export CCACHE_DIR=/mnt/build/ccache
export USE_CCACHE=1
source build/envsetup.sh
breakfast FP3
echo "=== START $(date -u) ==="
m systemimage vbmetaimage
ec=$?
echo "=== SYSIMG_EXIT=$ec $(date -u) ==="
echo "--- staged bootanimation Method (want: Stored) ---"
unzip -v out/target/product/FP3/system/product/media/bootanimation.zip 2>/dev/null | sed -n '3,6p'
ls -l out/target/product/FP3/system.img out/target/product/FP3/vbmeta.img
