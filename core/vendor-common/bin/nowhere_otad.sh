#!/system/bin/sh
# Nowhere on-device OTA updater (su:s0) -- DIA-20260618-03, "self-service OTA". On-demand worker (an app
# can't spawn su:s0): the caller writes $REQ then `setprop ctl.start nowhere_otad`; init runs this once and
# writes $RES. It asks the store whether a newer OS version is published and, if so, CDC-restores the payload
# (delta, via the chunk cache), applies it with update_engine, and reboots into the freshly written slot.
#   req (state tmpfs): present = check+apply (one line, the op, currently just "update")
#   res (state tmpfs): NONE | UPTODATE <ver> | STAGING | APPLYING <ver> | OK <ver> (reboot follows) | ERR-...
# The OS is public + content-addressed, so this carries NO profile creds (unlike nowhere_roamd).
STATE=/data/nowhere/state
REQ="$STATE/ota.req"
RES="$STATE/ota.res"
LOG=/data/nowhere/ota.log                 # status only
AGENT=/system/bin/nowhere_agent
PKG=/data/ota_package                      # update_engine staging dir -- NEVER rm/mkdir it (FBE policy); clear FILES only
# Per-edition OS payload ref. Each edition publishes its A/B payload under its OWN store ref -- the FP3
# (LineageOS) and Pixel (Endospore/GrapheneOS) images are NOT interchangeable, so a shared ref would cross
# the streams. The ref is baked per edition at /system/etc/nowhere/os-ref (the edition .mk PRODUCT_COPY_FILES
# it; same system-file label otad already reads nowhere.conf under, so no sepolicy change) and defaults to
# the FP3 ref so the shipping FP3 keeps working unchanged. The pass just keys the PUBLIC seal (carries no creds).
OSREF="$(cat /system/etc/nowhere/os-ref 2>/dev/null)"; OSREF="${OSREF:-os-fp3}"
OSPASS=nowhere-os

CONF=/data/nowhere/nowhere.conf
[ -f "$CONF" ] || CONF=/system/etc/nowhere/nowhere.conf
[ -f "$CONF" ] && . "$CONF"
export S3_ENDPOINT S3_REGION S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY
export SSL_CERT_DIR=/system/etc/security/cacerts
# The agent auto-discovers the device's own per-network DNS (privacy: no public-resolver leak), with a
# public fallback only if discovery fails. Set NOWHERE_DNS=host[:port] to force a specific resolver.
export NOWHERE_DNS="${NOWHERE_DNS:-}"
export NOWHERE_CHUNK_CACHE=/data/nowhere/otacache   # delta download: only fetch changed chunks (DIA-20260618-02)

set_res() { echo "$1" > "$RES" 2>/dev/null; chmod 600 "$RES" 2>/dev/null; }
rm -f "$REQ"                               # consume the trigger immediately

# Auto-started at the gate (sys.boot_completed) -- that can beat Wi-Fi association, so wait briefly for a
# default route before hitting the store (else ota-check would false-negative as "no version"). Best-effort:
# if no network in ~40s, fall through (the next boot retries).
i=0; while [ $i -lt 20 ]; do ip route show default 2>/dev/null | grep -q . && break; sleep 2; i=$((i+1)); done

# 1. Is a newer OS published?  ota-check exits 0 = update available, 1 = up to date, 3 = none/error.
out=$("$AGENT" ota-check s3 2>>"$LOG"); rc=$?
ver=$(echo "$out" | grep -oE '[0-9]+\.[0-9]+(\.[0-9]+)?' | tail -1)
if [ "$rc" = 1 ]; then echo "[otad] up to date $(date)" >> "$LOG"; set_res "UPTODATE ${ver:-?}"; exit 0; fi
if [ "$rc" != 0 ]; then echo "[otad] no published version (rc=$rc) $(date)" >> "$LOG"; set_res "NONE"; exit 0; fi
echo "[otad] update available -> $ver $(date)" >> "$LOG"; set_res "STAGING"

# 2. CDC-restore the payload (delta via the chunk cache). Clear the FILES in the staging dir, keep the dir.
rm -f "$PKG/payload.bin" "$PKG/payload_properties.txt"
if ! "$AGENT" restore s3 "$OSREF" "$OSPASS" "$PKG" >> "$LOG" 2>&1; then
  echo "[otad] restore FAILED $(date)" >> "$LOG"; set_res "ERR-RESTORE"; exit 0
fi
restorecon -R "$PKG" 2>> "$LOG"            # ota_package_file label; does NOT touch the FBE policy
[ -s "$PKG/payload.bin" ] && [ -s "$PKG/payload_properties.txt" ] || {
  echo "[otad] payload missing after restore $(date)" >> "$LOG"; set_res "ERR-PAYLOAD"; exit 0; }

# 3. Apply with update_engine, then reboot into the freshly written (inactive) slot. /data is untouched
# (a slot switch, not the amnesiac power-off wipe), so the user logs back into the same data on the new OS.
set_res "APPLYING ${ver:-?}"
update_engine_client --reset_status >> "$LOG" 2>&1
if update_engine_client --update --follow --payload="file://$PKG/payload.bin" \
       --headers="$(cat "$PKG/payload_properties.txt")" >> "$LOG" 2>&1; then
  echo "[otad] applied $ver -> rebooting $(date)" >> "$LOG"; set_res "OK ${ver:-?}"
  sync; reboot
else
  echo "[otad] update_engine FAILED $(date)" >> "$LOG"; set_res "ERR-APPLY"
fi
