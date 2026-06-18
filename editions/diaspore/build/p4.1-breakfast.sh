#!/usr/bin/env bash
# P4.1: verify the base tree, then `breakfast FP3` (roomservice pulls device/kernel/vendor) detached.
set -u
SRC=/mnt/build/lineage
LOG=/mnt/build/breakfast.log
echo "=== base tree sanity ==="
for d in build/make bionic frameworks/base system/core vendor/lineage; do
  [ -d "$SRC/$d" ] && echo "ok  $d" || echo "MISSING  $d"
done
cat > /mnt/build/run-breakfast.sh <<'EOF'
#!/usr/bin/env bash
cd /mnt/build/lineage
export PATH=$HOME/bin:$PATH
export LC_ALL=C
echo "=== breakfast START $(date) ==="
source build/envsetup.sh
breakfast FP3
echo "=== BREAKFAST_EXIT=$? $(date) ==="
EOF
chmod +x /mnt/build/run-breakfast.sh
setsid bash /mnt/build/run-breakfast.sh > "$LOG" 2>&1 < /dev/null &
disown 2>/dev/null || true
sleep 6
echo "=== breakfast launched; log so far ==="; tail -20 "$LOG" 2>/dev/null
echo P4_1_BREAKFAST_LAUNCHED
