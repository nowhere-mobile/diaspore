#!/usr/bin/env bash
# P4.2: rebuild the agent module (install the new S3 agent). No `set -u` (envsetup/breakfast aren't safe under it).
set -o pipefail
cat > /mnt/build/run-rebuild.sh <<'EOF'
#!/usr/bin/env bash
cd /mnt/build/lineage
export PATH=$HOME/bin:$PATH LC_ALL=C USE_CCACHE=1 CCACHE_DIR=/mnt/build/ccache
source build/envsetup.sh
breakfast FP3
echo "=== m diaspore_agent START $(date) combo=$TARGET_PRODUCT-$TARGET_RELEASE-$TARGET_BUILD_VARIANT ==="
m diaspore_agent
echo "=== REBUILD_EXIT=$? $(date) ==="
ls -la out/target/product/FP3/system/bin/diaspore_agent
EOF
chmod +x /mnt/build/run-rebuild.sh
pkill -f run-mbuild.sh 2>/dev/null || true
setsid bash /mnt/build/run-rebuild.sh > /mnt/build/rebuild.log 2>&1 < /dev/null &
disown 2>/dev/null || true
sleep 6
echo "=== launched; log so far ==="; tail -10 /mnt/build/rebuild.log
echo P4_2_REBUILD_LAUNCHED
