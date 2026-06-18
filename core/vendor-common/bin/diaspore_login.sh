#!/system/bin/sh
# Diaspore blind-login daemon launcher (P3.1). init creates the AF_UNIX socket diaspore_login and
# passes its fd (env ANDROID_SOCKET_diaspore_login); we source the store config + env and exec the
# agent's login-daemon, which reads name/pass from the chooser over that socket and restores the
# chosen profile's working set into the tmpfs IN-PROCESS. The passphrase only ever lives in the
# daemon's memory -- never on disk, never in any argv.
LOG=/data/diaspore/login.log
mkdir -p /data/diaspore
# Store config: prefer the provisioned, mutable /data copy (kept out of the read-only system image and
# rotatable without a reflash); fall back to a baked /system copy for older / un-migrated devices.
# IMPORTANT (Tier 2): start the daemon even with NO conf -- a fresh free-OS flash ships with no store, and
# the Settings/Store screen talks to THIS daemon (GET-STORE / SET-STORE) to configure one in-place. With no
# store the daemon answers create/login with NOSTORE (gate -> Settings); the worker re-sources the conf each
# run, so a SET-STORE takes effect without a reboot.
CONF=/data/diaspore/diaspore.conf
[ -f "$CONF" ] || CONF=/system/etc/diaspore/diaspore.conf
if [ -f "$CONF" ]; then
  . "$CONF"
  export S3_ENDPOINT S3_REGION S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY
  echo "[login] store config loaded $(date)" >> "$LOG"
else
  echo "[login] no store configured yet -- daemon up for Settings/Store $(date)" >> "$LOG"
fi
# Discovery endpoint (baked default): lets a fresh device with NO store bootstrap its data-store config from
# name+passphrase (the daemon GETs a sealed config at an unguessable bootstrapRef). Sealed configs only ->
# the discovery endpoint sees ciphertext (zero-knowledge). Prefer the /data copy; fall back to a baked one.
DCONF=/data/diaspore/discovery.conf
[ -f "$DCONF" ] || DCONF=/system/etc/diaspore/discovery.conf
if [ -f "$DCONF" ]; then
  . "$DCONF"
  export DISCO_ENDPOINT DISCO_REGION DISCO_BUCKET DISCO_ACCESS_KEY DISCO_SECRET_KEY
  echo "[login] discovery endpoint loaded $(date)" >> "$LOG"
fi
export SSL_CERT_DIR=/system/etc/security/cacerts
export DIASPORE_DNS="${DIASPORE_DNS:-1.1.1.1:53}"
export DIASPORE_STATE=/data/diaspore/state
echo "[login] daemon start $(date)" >> "$LOG"
exec /system/bin/diaspore_agent login-daemon >> "$LOG" 2>&1
