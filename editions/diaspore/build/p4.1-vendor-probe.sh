#!/usr/bin/env bash
# Find the correct TheMuppets vendor repo + branch for FP3 (proprietary blobs).
for r in \
  https://github.com/TheMuppets/proprietary_vendor_fairphone.git \
  https://github.com/TheMuppets/proprietary_vendor_fairphone_FP3.git \
  https://gitlab.com/the-muppets/proprietary_vendor_fairphone.git \
  https://github.com/TheMuppets/proprietary_vendor_fairphone_sdm632.git ; do
  echo "===== $r ====="
  git ls-remote --heads "$r" 2>&1 | grep -E 'lineage-2[0-9]' | head -8 || echo "  (none/err)"
done
echo PROBE_DONE
