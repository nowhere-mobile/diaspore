#!/usr/bin/env bash
# P4.2 (fix): Android 15 lunch needs <product>-<release>-<variant>; use breakfast FP3 to set the combo.
set -u
pkill -f run-mbuild.sh 2>/dev/null || true; sleep 1
cat > /mnt/build/run-mbuild.sh <<'EOF'
#!/usr/bin/env bash
cd /mnt/build/lineage
export PATH=$HOME/bin:$PATH LC_ALL=C USE_CCACHE=1 CCACHE_DIR=/mnt/build/ccache
source build/envsetup.sh
breakfast FP3 >/dev/null 2>&1
echo "=== m diaspore modules START $(date) (combo: $TARGET_PRODUCT-$TARGET_RELEASE-$TARGET_BUILD_VARIANT) ==="
m diaspore_agent diaspore.rc diaspore_boot.sh diaspore_shutdown.sh
echo "=== MBUILD_EXIT=$? $(date) ==="
echo "--- installed into system? ---"
ls -la out/target/product/FP3/system/bin/diaspore_agent \
       out/target/product/FP3/system/bin/diaspore_boot.sh \
       out/target/product/FP3/system/bin/diaspore_shutdown.sh \
       out/target/product/FP3/system/etc/init/diaspore.rc 2>&1
EOF
chmod +x /mnt/build/run-mbuild.sh
setsid bash /mnt/build/run-mbuild.sh > /mnt/build/mbuild.log 2>&1 < /dev/null &
disown 2>/dev/null || true
sleep 6
echo "=== relaunched; log so far ==="; tail -15 /mnt/build/mbuild.log
echo P4_2_MBUILD_RELAUNCHED
