#!/bin/bash
# Make LineageOS install OUR boot animation instead of generating its own, WITHOUT a duplicate
# install rule. The lineage_bootanimation soong genrule (vendor/lineage/bootanimation/Android.bp)
# copies $(TARGET_BOOTANIMATION) when that's set, so just point it at our prebuilt in the FP3
# BoardConfig (idempotent). The screen dims guarantee soong's select() takes the prebuilt branch.
# (Do NOT PRODUCT_COPY_FILES to /product/media/bootanimation.zip — that collides with the module.)
BC=/mnt/build/lineage/device/fairphone/FP3/BoardConfig.mk
if grep -q "TARGET_BOOTANIMATION :=" "$BC"; then
  echo "BoardConfig.mk: TARGET_BOOTANIMATION already present (skip)"
else
  cat >> "$BC" <<'EOF'

# Diaspore: use our prebuilt boot animation (picked up by the lineage_bootanimation soong module).
TARGET_BOOTANIMATION := vendor/diaspore/media/bootanimation.zip
TARGET_SCREEN_WIDTH ?= 1080
TARGET_SCREEN_HEIGHT ?= 2160
EOF
  echo "BoardConfig.mk: added TARGET_BOOTANIMATION"
fi
echo "--- tail ---"; tail -6 "$BC"
