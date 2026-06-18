#!/usr/bin/env bash
# P4.1 -> first vanilla image: breakfast FP3 then brunch FP3, as ONE detached pipeline.
# Survives ssh disconnects + local reboots (setsid). Logs to /mnt/build/build.log; ends with BUILD_EXIT.
set -u
LOG=/mnt/build/build.log
# stop the standalone breakfast (we re-run it as the head of the chained pipeline)
pkill -f run-breakfast.sh 2>/dev/null || true
sleep 1
cat > /mnt/build/run-build.sh <<'EOF'
#!/usr/bin/env bash
cd /mnt/build/lineage
export PATH=$HOME/bin:$PATH
export LC_ALL=C
export USE_CCACHE=1
export CCACHE_DIR=/mnt/build/ccache
export CCACHE_EXEC=$(command -v ccache)
mkdir -p "$CCACHE_DIR"
ccache -M 50G >/dev/null 2>&1 || true
echo "=== BREAKFAST START $(date) ==="
source build/envsetup.sh
breakfast FP3 || { echo "=== BREAKFAST_FAILED rc=$? $(date) ==="; exit 11; }
echo "=== BRUNCH START $(date) ==="
brunch FP3
echo "=== BUILD_EXIT=$? $(date) ==="
EOF
chmod +x /mnt/build/run-build.sh
setsid bash /mnt/build/run-build.sh > "$LOG" 2>&1 < /dev/null &
disown 2>/dev/null || true
sleep 6
echo "=== pipeline launched; log so far ==="; tail -20 "$LOG" 2>/dev/null
echo "=== proc ==="; pgrep -af run-build.sh | head
echo P4_1_BUILD_LAUNCHED
