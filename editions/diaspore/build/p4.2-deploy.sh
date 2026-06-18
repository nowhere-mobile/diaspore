#!/usr/bin/env bash
# P4.2: deploy the vendor/diaspore module into the FP3 tree, hook it, and detached-build the modules.
set -uxo pipefail
SRC=/mnt/build/lineage
VD=$SRC/vendor/diaspore
# arm64 agent straight into the module (robust; no /tmp dependency)
( cd /home/chesterr/phase2/agent && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$VD/bin/diaspore_agent" . )
chmod 755 "$VD/bin/diaspore_agent" "$VD/bin/diaspore_boot.sh" "$VD/bin/diaspore_shutdown.sh"
echo "=== vendor/diaspore ==="; find "$VD" -type f | sort; file "$VD/bin/diaspore_agent"
# hook into the FP3 product (idempotent)
MK="$SRC/device/fairphone/FP3/lineage_FP3.mk"
grep -q 'vendor/diaspore/diaspore.mk' "$MK" || printf '\n# Diaspore system-side components (P4.2)\n$(call inherit-product-if-exists, vendor/diaspore/diaspore.mk)\n' >> "$MK"
echo "=== lineage_FP3.mk tail ==="; tail -3 "$MK"
# detached module build to validate Android.bp + installation
cat > /mnt/build/run-mbuild.sh <<'EOF'
#!/usr/bin/env bash
cd /mnt/build/lineage
export PATH=$HOME/bin:$PATH LC_ALL=C USE_CCACHE=1 CCACHE_DIR=/mnt/build/ccache
source build/envsetup.sh
lunch lineage_FP3-userdebug
echo "=== m diaspore modules START $(date) ==="
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
sleep 5
echo "=== mbuild launched; log so far ==="; tail -12 /mnt/build/mbuild.log
echo P4_2_DEPLOY_DONE
