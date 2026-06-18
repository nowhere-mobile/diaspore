#!/usr/bin/env bash
# P4.2 recon: Go toolchain + agent source, cross-compile agent for arm64, FP3 product makefile layout.
SRC=/mnt/build/lineage
echo "=== go toolchain ==="
which go; go version 2>&1 | head -1
echo "=== agent source ==="
ls -la /home/chesterr/phase2/agent/ 2>/dev/null | head
echo "=== cross-compile agent for arm64 (Android = linux/arm64, static) ==="
( cd /home/chesterr/phase2/agent && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/diaspore_agent_arm64 . && file /tmp/diaspore_agent_arm64 ) 2>&1 | tail -4
echo "=== FP3 product makefiles ==="
ls -la "$SRC/device/fairphone/FP3/"*.mk 2>/dev/null
echo "=== which .mk defines PRODUCT_NAME lineage_FP3 (our inheritance point) ==="
grep -l 'lineage_FP3' "$SRC/device/fairphone/FP3/"*.mk 2>/dev/null
echo "=== head of lineage_FP3.mk ==="
sed -n '1,45p' "$SRC/device/fairphone/FP3/lineage_FP3.mk" 2>/dev/null || echo "(no lineage_FP3.mk)"
echo P4_2_RECON_DONE
