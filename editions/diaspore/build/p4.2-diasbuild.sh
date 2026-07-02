#!/usr/bin/env bash
# P4.2: full brunch FP3 -> the first Diaspore-flavored FP3 image (agent + S3 hooks + baked config).
set -o pipefail
cat > /mnt/build/run-diasbuild.sh <<'EOF'
#!/usr/bin/env bash
cd /mnt/build/lineage
export PATH=$HOME/bin:$PATH LC_ALL=C USE_CCACHE=1 CCACHE_DIR=/mnt/build/ccache
source build/envsetup.sh
breakfast FP3
echo "=== DIASPORE BRUNCH START $(date) ==="
brunch FP3
echo "=== DIASBUILD_EXIT=$? $(date) ==="
echo "--- OTA zip + baked diaspore files ---"
ls -la out/target/product/FP3/lineage-*.zip \
       out/target/product/FP3/system/etc/nowhere/nowhere.conf \
       out/target/product/FP3/system/bin/nowhere_agent \
       out/target/product/FP3/system/bin/diaspore_boot.sh \
       out/target/product/FP3/system/etc/init/nowhere.rc 2>&1
EOF
chmod +x /mnt/build/run-diasbuild.sh
pkill -f run-rebuild.sh 2>/dev/null || true; pkill -f run-mbuild.sh 2>/dev/null || true
setsid bash /mnt/build/run-diasbuild.sh > /mnt/build/diasbuild.log 2>&1 < /dev/null &
disown 2>/dev/null || true
sleep 8
echo "=== launched; log so far ==="; tail -12 /mnt/build/diasbuild.log
echo DIASBUILD_LAUNCHED
