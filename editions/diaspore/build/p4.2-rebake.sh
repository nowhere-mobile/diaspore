#!/usr/bin/env bash
# P4.2: re-cross-compile the NEW S3-capable agent into vendor/diaspore and rebuild the module.
set -uxo pipefail
SRC=/mnt/build/lineage
VD=$SRC/vendor/diaspore
echo "=== re-cross-compile S3-capable agent (arm64, static) -> module ==="
( cd /home/chesterr/phase2/agent && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$VD/bin/diaspore_agent" . )
ls -la "$VD/bin/diaspore_agent"; file "$VD/bin/diaspore_agent"
echo "=== rebuild the agent module (same env so Soong reuses its analysis) ==="
cd "$SRC"
export PATH=$HOME/bin:$PATH LC_ALL=C USE_CCACHE=1 CCACHE_DIR=/mnt/build/ccache
source build/envsetup.sh >/dev/null 2>&1
breakfast FP3 >/dev/null 2>&1
echo "combo=$TARGET_PRODUCT-$TARGET_RELEASE-$TARGET_BUILD_VARIANT"
m diaspore_agent 2>&1 | tail -15
echo "=== installed agent (should be the larger S3 build) ==="
ls -la out/target/product/FP3/system/bin/diaspore_agent
echo P4_2_REBAKE_DONE
