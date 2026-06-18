#!/bin/bash
# Wire the Diaspore confined sepolicy into the FP3 build (idempotent). The domain runs /system
# binaries, so it belongs in the system-side private policy: SYSTEM_EXT_PRIVATE_SEPOLICY_DIRS
# (BOARD_PLAT_PRIVATE_SEPOLICY_DIR is obsolete in this tree). The dir vendor/diaspore/sepolicy holds
# diaspore.te + file_contexts.
BC=/mnt/build/lineage/device/fairphone/FP3/BoardConfig.mk
if grep -q "vendor/diaspore/sepolicy" "$BC"; then
  echo "BoardConfig.mk: sepolicy already wired (skip)"
else
  cat >> "$BC" <<'EOF'

# Diaspore confined SELinux domain (system-side private policy).
SYSTEM_EXT_PRIVATE_SEPOLICY_DIRS += vendor/diaspore/sepolicy
EOF
  echo "BoardConfig.mk: added SYSTEM_EXT_PRIVATE_SEPOLICY_DIRS += vendor/diaspore/sepolicy"
fi
tail -4 "$BC"
