#!/usr/bin/env bash
# P4.2b: rebuild the diaspore boot/shutdown script modules (now S3-config-driven). No set -u (envsetup unsafe).
SRC=/mnt/build/lineage
chmod 755 "$SRC/vendor/diaspore/bin/diaspore_boot.sh" "$SRC/vendor/diaspore/bin/diaspore_shutdown.sh"
cd "$SRC"
export PATH=$HOME/bin:$PATH LC_ALL=C USE_CCACHE=1 CCACHE_DIR=/mnt/build/ccache
source build/envsetup.sh >/dev/null 2>&1
breakfast FP3 >/dev/null 2>&1
echo "combo=${TARGET_PRODUCT:-?}-${TARGET_RELEASE:-?}-${TARGET_BUILD_VARIANT:-?}"
m diaspore_boot.sh diaspore_shutdown.sh 2>&1 | tail -12
echo "=== installed ==="
ls -la out/target/product/FP3/system/bin/diaspore_boot.sh out/target/product/FP3/system/bin/diaspore_shutdown.sh 2>&1
echo "=== S3 mode present in installed boot script? ==="
grep -c 'restore-set s3' out/target/product/FP3/system/bin/diaspore_boot.sh 2>/dev/null
echo P4_2B_SCRIPTS_DONE
