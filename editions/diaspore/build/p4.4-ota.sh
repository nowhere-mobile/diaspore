#!/usr/bin/env bash
# P4.4 — OS OTA on the LOCKED device (DIA-20260613-08). REFERENCE RUNBOOK.
#
# Proves the update path for a locked verified-boot A/B device: you can no longer `fastboot flash` it,
# so updates go as a signed payload applied by update_engine to the inactive slot. This worked on the
# locked FP3 with NO unlock: a testkey-signed full OTA was accepted by update_engine, written to the
# inactive slot, AVB-verified under our custom key (stays yellow), slot-switched, and booted as v2;
# A/B rollback both ways works; /data is untouched (roaming + conf survive).
#
# Why it works on a locked device: update_engine verifies the OTA *package* against the testkey
# (otacerts.zip = testkey), which is the build default / compiled-in key -> no payload-key file needed.
# The *images inside* the payload are AVB-signed with our custom Diaspore key (BoardConfig), so they
# verify under avb_custom_key at boot -> yellow. The two key systems are independent.
#
# The content-addressed DELTA TRANSPORT (P3.3 vision: pull the payload as CDC chunks from the store
# instead of a local file) is the follow-on P4.4b -- it only changes how the payload bytes arrive, not
# the apply/verify/rollback proven here.
set -u
TREE=/mnt/build/lineage
OUT=$TREE/out/target/product/FP3
ADB="adb"   # host platform-tools

cat <<'EOF'
### 1. Build a v2 OTA package on the build host
  # A detectable v2: vendor/diaspore/diaspore.mk stamps vendor/diaspore/etc/diaspore-ota-version into
  # /system/etc/diaspore-ota-version (v1 lacked it). Then:
  cd /mnt/build && source lineage/build/envsetup.sh && breakfast FP3 && m otapackage
  # -> out/target/product/FP3/lineage_FP3-ota.zip  (full A/B OTA, signed with the testkey ~1GB)

### 2. Extract the payload (payload.bin is stored uncompressed in the zip)
  unzip -o $OUT/lineage_FP3-ota.zip payload.bin payload_properties.txt -d /tmp/otax
  # payload_properties.txt holds FILE_HASH/FILE_SIZE/METADATA_HASH/METADATA_SIZE = the --headers below.

### 3. Get the payload onto the device (locked device: no fastboot; update_engine reads a local file)
  # pull /tmp/otax/payload.bin to the host, then:
  adb root
  adb shell 'mkdir -p /data/ota_package'
  adb push payload.bin            /data/ota_package/payload.bin
  adb push payload_properties.txt /data/ota_package/payload_properties.txt
  adb shell 'restorecon -R /data/ota_package'   # -> u:object_r:ota_package_file:s0

### 4. Apply via update_engine (verifies the testkey signature first -- our gate -- then writes _inactive_)
  adb shell 'update_engine_client --reset_status'
  adb shell 'update_engine_client --update --follow \
      --payload=file:///data/ota_package/payload.bin \
      --headers="$(cat /data/ota_package/payload_properties.txt)"'
  # ends at onPayloadApplicationComplete(kSuccess) -> UPDATE_STATUS_UPDATED_NEED_REBOOT. ~5 min for 1GB.

### 5. Reboot into the updated slot + verify
  adb reboot
  # confirm: ro.boot.slot_suffix flipped; /system/etc/diaspore-ota-version present (= v2);
  # verifiedbootstate=yellow, flash.locked=1, veritymode=enforcing (OTA'd images verify under our key);
  # /data intact (adb auth + /data/diaspore/diaspore.conf survive -- OTA does NOT wipe /data).
  # update_verifier marks the slot successful on a good boot (bootctl is-slot-marked-successful <n> -> 1).

### 6. A/B rollback (safety net; both slots are valid Diaspore)
  adb shell 'bootctl set-active-boot-slot 1'; adb reboot   # -> v1 on _b (marker absent), still yellow
  adb shell 'bootctl set-active-boot-slot 0'; adb reboot   # -> v2 on _a (marker 0.1.0), still yellow

### Notes
# - Full payload here; incremental (delta) OTA = `ota_from_target_files -i <v1-target-files> ...`.
# - A bad update is non-fatal: A/B + the bootloader's retry roll back to the last-good slot.
# - update_engine never touches /data, so an OS update keeps the user's roaming state + device config.
EOF
