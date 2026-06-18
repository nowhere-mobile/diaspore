#!/usr/bin/env bash
# P4.1: add TheMuppets FP3 vendor blobs (local manifest) + sync, then relaunch the build pipeline.
set -u
SRC=/mnt/build/lineage
export PATH=$HOME/bin:$PATH
mkdir -p "$SRC/.repo/local_manifests"
cat > "$SRC/.repo/local_manifests/fp3-vendor.xml" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="themuppets-gh" fetch="https://github.com/TheMuppets" />
  <project name="proprietary_vendor_fairphone_FP3" path="vendor/fairphone/FP3" remote="themuppets-gh" revision="lineage-22.2" />
</manifest>
EOF
cd "$SRC"
echo "=== sync vendor project (git-lfs) ==="
repo sync -c --force-sync --no-clone-bundle vendor/fairphone/FP3 2>&1 | tail -15
echo "=== ensure LFS blobs are real (not pointers) ==="
( cd "$SRC/vendor/fairphone/FP3" && git lfs pull 2>&1 | tail -3 ) || true
echo "=== verify ==="
if [ -f "$SRC/vendor/fairphone/FP3/FP3-vendor.mk" ]; then
  echo "VENDOR_OK size=$(du -sh "$SRC/vendor/fairphone/FP3" | cut -f1)"
  echo "=== relaunch detached breakfast -> brunch ==="
  bash /home/chesterr/phase4/build/p4.1-build.sh
else
  echo "VENDOR_MISSING — not launching build"
fi
echo P4_1_VENDOR_DONE
