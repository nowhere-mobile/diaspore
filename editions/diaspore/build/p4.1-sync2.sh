#!/usr/bin/env bash
# Phase 4 / P4.1: relaunch repo sync DETACHED (setsid) so it survives ssh disconnects.
# Resumes the partial sync. Logs to /mnt/build/sync.log (poll that for progress / SYNC_EXIT).
set -u
LOG=/mnt/build/sync.log
RUN=/mnt/build/run-sync.sh
pkill -f 'repo sync' 2>/dev/null || true; sleep 1
cat > "$RUN" <<'EOF'
#!/usr/bin/env bash
cd /mnt/build/lineage
export PATH=$HOME/bin:$PATH
echo "=== START $(date) ==="
repo sync -c -j16 --force-sync --no-clone-bundle --no-tags
echo "=== SYNC_EXIT=$? $(date) ==="
EOF
chmod +x "$RUN"
setsid bash "$RUN" > "$LOG" 2>&1 < /dev/null &
disown 2>/dev/null || true
sleep 4
echo "=== detached pid(s) ==="; pgrep -af run-sync.sh | head; pgrep -afc git || true
echo "=== log head ==="; head -5 "$LOG" 2>/dev/null
echo "=== df ==="; df -h /mnt/build | tail -1
echo P4_1_SYNC_RELAUNCHED
