#!/usr/bin/env bash
# P4.3 — Verified boot under a custom Diaspore key (DIA-20260613-07). REFERENCE RUNBOOK.
#
# This is the procedure that actually works on the FP3, recorded because the device tree and the
# signing keys live OUTSIDE this repo (the FP3 device tree is a separate LineageOS repo on the build
# host; the keys are signing secrets kept out-of-band). It is part-automatable / part-manual: the
# bootloader lock/unlock and the post-lock recovery factory-reset require PHYSICAL confirmation on the
# device and cannot be scripted. Run it section by section, not as one shot.
#
# Outcome: verifiedbootstate=yellow, flash.locked=1, veritymode=enforcing — the bootloader refuses to
# boot anything not signed by the Diaspore key, and dm-verity enforces /system.
#
# !!! SECURITY: the custom AVB *private* keys are the device's root of trust. NEVER commit them. Keep
# !!! them out-of-band and BACK THEM UP — losing them means no updates the locked device will accept
# !!! (recovery = unlock + wipe). They live on the build host at $TREE/diaspore-keys (gitignored by
# !!! virtue of not being a manifest project; do not move them under vendor/diaspore).
set -u
TREE=/mnt/build/lineage                       # LineageOS build tree (build host)
KEYS=$TREE/diaspore-keys                       # in-tree (Soong needs AVB key paths inside the tree),
                                               # but OUTSIDE vendor/diaspore so they're never in our repo
OUT=$TREE/out/target/product/FP3
BC=$TREE/device/fairphone/FP3/BoardConfig.mk   # device tree (separate repo; these edits are uncommitted there)

echo "### 1. Custom keys (once; back these up!)"
echo "mkdir -p $KEYS && chmod 700 $KEYS"
echo "openssl genrsa -out $KEYS/diaspore_avb_rsa4096.pem 4096   # main vbmeta key (= avb_custom_key root)"
echo "openssl genrsa -out $KEYS/diaspore_avb_rsa2048.pem 2048   # system chain key"
echo "(cd $TREE/external/avb && python3 avbtool.py extract_public_key \\"
echo "   --key $KEYS/diaspore_avb_rsa4096.pem --output $KEYS/diaspore_avb_pub.bin)"

cat <<'EOF'

### 2. BoardConfig.mk (device/fairphone/FP3) — the exact diff
  # was: signed with AOSP test keys + verity disabled
  -BOARD_AVB_SYSTEM_KEY_PATH := external/avb/test/data/testkey_rsa2048.pem
  -# Disable verity and descriptor checking
  -BOARD_AVB_MAKE_VBMETA_IMAGE_ARGS += --set_hashtree_disabled_flag
  # now: custom Diaspore keys (tree-relative paths) + verity ENABLED (flag removed)
  +BOARD_AVB_KEY_PATH := diaspore-keys/diaspore_avb_rsa4096.pem
  +BOARD_AVB_ALGORITHM := SHA256_RSA4096
  +BOARD_AVB_SYSTEM_KEY_PATH := diaspore-keys/diaspore_avb_rsa2048.pem
  # (BOARD_AVB_SYSTEM_ALGORITHM stays SHA256_RSA2048)

### 3. Build + sanity
  cd /mnt/build && bash p4.5-sysimg.sh                 # m systemimage vbmetaimage
  # verify: vbmeta Flags:0 (verity on) + the custom pubkey + chain->custom system key:
  (cd $TREE/external/avb && python3 avbtool.py info_image --image $OUT/vbmeta.img | \
     grep -iE 'Public key|^Flags|Partition Name')

### 4. Flash BOTH slots (critical — else an A/B fallback lands on the stock Fairphone slot), then lock.
###    All fastboot here; the device must be UNLOCKED to flash.
  fastboot flash avb_custom_key  $OUT/../../../diaspore-keys/diaspore_avb_pub.bin   # (use the pulled pub.bin)
  for s in a b; do
    fastboot flash boot_$s   boot.img
    fastboot flash dtbo_$s   dtbo.img
    fastboot flash vendor_$s vendor.img
    fastboot flash system_$s system.img
    fastboot flash vbmeta_$s vbmeta.img
  done
  fastboot -w                      # format /data clean (so the pre-lock unlocked boot comes up clean)
  fastboot --set-active=b
  fastboot reboot
  # >>> VERIFY UNLOCKED FIRST: boots, ro.boot.veritymode=enforcing, full roam cycle works.
  #     HARD GATE — do not lock unless this passes.

### 5. Lock (PHYSICAL confirm on device)
  adb reboot bootloader
  fastboot flashing lock           # confirm on the device screen (Volume + Power)
  # The lock wipes /data; first boot shows recovery "cannot load Android system" -> on the device pick
  # "Factory data reset" -> reboot. It comes up Diaspore (both slots are Diaspore) and self-provisions
  # the device owner / gate. Re-enable USB debugging (fresh /data) to regain adb.

### 6. Re-provision + verify
  adb push <diaspore.conf> /data/diaspore/diaspore.conf   # /data was wiped; conf is out-of-band
  # confirm: ro.boot.verifiedbootstate=yellow, flash.locked=1, veritymode=enforcing, roam login works.

### RECOVERY (if a locked boot ever fails): fastboot flashing unlock (wipes /data) -> orange/working.
### NOTE: once locked, /system can no longer be hot-pushed or fastboot-flashed; dev iteration on this
### device needs unlock -> reflash -> re-lock.
EOF
