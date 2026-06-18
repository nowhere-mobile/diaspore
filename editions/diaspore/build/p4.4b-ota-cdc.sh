#!/usr/bin/env bash
# P4.4b — Content-addressed OTA transport (DIA-20260613-09). REFERENCE RUNBOOK.
#
# The P3.3/Diaspore vision: the OS roams through the SAME content-addressed store as user data. This
# layers on top of the working update_engine OTA (P4.4): instead of adb-pushing the payload, the OS
# payload is pushed to the store as CDC chunks (the agent's existing push/restore -- NO new code), and
# the device fetches it via CDC restore, then update_engine applies it. Proven end-to-end on the LOCKED
# FP3 with no unlock: 780 chunks, byte-identical on arrival, applied, booted v2 yellow.
#
# WHY this is "free": the agent already does content-defined chunking + convergent-sealed dedup for user
# data (DIA-...-05). An OTA payload is just bytes -- push a dir holding it, restore it on the device. A
# v2->v3 payload that shares most bytes dedups on push (store-side), so the store only grows by the delta.
set -u
AGENT_HOST=/tmp/agent_host        # the agent built for the host (GOOS=linux native), same source as the device's
OSREF=os-fp3 ; OSPASS=diaspore-os # a well-known ref for the OS payload (the OS is public; pass just keys the seal)

cat <<'EOF'
### 1. Publish the OTA payload to the store as CDC chunks (build host)
  # payload.bin + payload_properties.txt come from `m otapackage` -> unzip (see p4.4-ota.sh steps 1-2).
  # Put them in a dir and push via the agent's CDC (sources S3_* from diaspore.conf):
  . /mnt/build/lineage/vendor/diaspore/etc/diaspore.conf
  export S3_ENDPOINT S3_REGION S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY
  /tmp/agent_host push s3 os-fp3 diaspore-os /tmp/otax      # /tmp/otax = {payload.bin, payload_properties.txt}
  # -> "push: profile os-fp3 -> N chunks". A re-push fully dedups (content-addressed): N+1 dedup, 0 new.
  # Publish the target OS version so the on-device updater's `ota-check` knows an update exists (DIA-...-03).
  # Must be > the running /system/etc/diaspore-ota-version for the device to act.
  /tmp/agent_host ota-mark s3 0.2.0

### 2. Fetch it onto the device via CDC restore  (LOCKED device, no unlock; the agent is already baked)
  adb root
  # !!! DO NOT `rm -rf` / `mkdir` /data/ota_package -- it's a system dir with a specific FBE encryption
  # !!! policy. Recreating it makes the NEXT boot fail `set_policy_failed: /data/ota_package` -> recovery
  # !!! -> only a factory reset clears it. Clear the FILES only; keep the dir:
  adb shell 'rm -f /data/ota_package/payload.bin /data/ota_package/payload_properties.txt'
  # DIASPORE_CHUNK_CACHE (DIA-20260618-02) = device-side delta download: chunks are cached by content hash
  # under /data/diaspore/otacache (ciphertext-only, like the store), so a later v2->v3 restore network-fetches
  # ONLY the changed chunks (the restore prints "N chunks: C cached, F fetched"). The cache persists across the
  # OTA reboot; a full power-off (amnesiac wipe) clears it (then the next OTA simply repopulates it). NOT the
  # /data/ota_package dir above -- a plain cache dir, safe to create/clear.
  adb shell '. /data/diaspore/diaspore.conf; export S3_ENDPOINT S3_REGION S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY; \
             export SSL_CERT_DIR=/system/etc/security/cacerts; export DIASPORE_DNS=1.1.1.1:53; \
             export DIASPORE_CHUNK_CACHE=/data/diaspore/otacache; \
             /system/bin/diaspore_agent restore s3 os-fp3 diaspore-os /data/ota_package'
  adb shell 'restorecon -R /data/ota_package'    # SELinux label (ota_package_file); does NOT touch FBE policy
  # verify byte-identical: device sha256 of payload.bin == the host original.

### 3. Apply + reboot (same as P4.4, just a CDC-delivered payload now)
  adb shell 'update_engine_client --reset_status'
  adb shell 'update_engine_client --update --follow --payload=file:///data/ota_package/payload.bin \
             --headers="$(cat /data/ota_package/payload_properties.txt)"'
  adb reboot
  # -> boots the flipped slot, v2, verifiedbootstate=yellow, veritymode=enforcing, boot_completed=1 (clean).

### Self-service path (DIA-20260618-03): the on-device updater does steps 2-3 with NO computer.
  # The diaspore_otad service (su:s0, /system/bin/diaspore_otad.sh) runs ota-check (vs step 1's ota-mark
  # version), and if newer, CDC-restores the payload (DELTA via the chunk cache), applies it with
  # update_engine, and reboots into the new slot. Trigger it on-demand (a UX trigger -- gate "Check for
  # updates" -- is the 2b follow-on):
  adb shell 'mkdir -p /data/diaspore/state; echo update > /data/diaspore/state/ota.req; setprop ctl.start diaspore_otad'
  adb shell 'cat /data/diaspore/state/ota.res; cat /data/diaspore/ota.log'   # NONE|UPTODATE|STAGING|APPLYING|OK <ver>
  # OK <ver> -> the device reboots itself into the flipped slot, v<ver>, /data intact.

### Notes
# - Device-side DELTA download is DONE (DIA-20260618-02): diaspore_otad sets DIASPORE_CHUNK_CACHE so the
#   restore only fetches chunks it doesn't already have (the restore prints "N chunks: C cached, F fetched").
# - The manual steps 2-3 above remain the build-host/adb reference path; diaspore_otad is the on-device one.
#   Both live in /system, so a new updater ships in a future image (needs unlock->reflash on the locked dev
#   device).
EOF
