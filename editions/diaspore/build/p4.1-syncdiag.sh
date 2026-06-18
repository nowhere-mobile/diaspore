#!/usr/bin/env bash
echo "=== sync script alive? ==="
pgrep -af p4.1-sync.sh || echo "p4.1-sync.sh NOT running"
echo "=== repo/git/python processes ==="
ps -eo pid,ppid,etimes,comm,args | grep -iE 'repo|git-remote|python3' | grep -v grep | head -20 || echo "none"
echo "=== disk write activity in last 2 min? ==="
n=$(find /mnt/build/lineage/.repo -type f -newermt '-2 minutes' 2>/dev/null | wc -l)
echo "files modified in last 2 min: $n"
echo "=== df ==="; df -h /mnt/build | tail -1
echo "=== repo sync state file ==="
ls -la /mnt/build/lineage/.repo/.repo_fetchtimes.json 2>/dev/null
cat /mnt/build/lineage/.repo/project.list 2>/dev/null | wc -l
echo "=== uptime/load ==="; uptime
echo DIAG_DONE
