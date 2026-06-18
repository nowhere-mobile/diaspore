#!/system/bin/sh
# Diaspore Arc 2 per-user DATA worker (su:s0). The chooser (device owner) owns the user LIFECYCLE
# (create/start/switch/stop/remove via DPM/AM -- M1); THIS does only the privileged DATA half the
# confined diaspore domain can't: restore arbitrary app data into /data/user/N + restorecon, and seal it
# back. On-demand -- an app can't spawn su:s0, so the caller writes the request file into the RAM tmpfs
# then `setprop ctl.start diaspore_roamd`; init runs this once; it writes the result file.
#   req (state tmpfs = RAM, consumed immediately):  op\nname\npass\nuid
#   res (state tmpfs = RAM):                         OK <uid> | BLANK | ERR-...
# req/res live in /data/diaspore/state (the tmpfs, gone on power-off; the sync skips plain files, so the
# creds never roam) -- same trick as the Arc-1 .session marker.
STATE=/data/diaspore/state
REQ="$STATE/roam.req"
RES="$STATE/roam.res"
LOG=/data/diaspore/roam.log            # status only -- never creds
AGENT=/system/bin/diaspore_agent
# Store config: prefer the provisioned, mutable /data copy (out of the read-only image + rotatable);
# fall back to a baked /system copy for older / un-migrated devices.
CONF=/data/diaspore/diaspore.conf
[ -f "$CONF" ] || CONF=/system/etc/diaspore/diaspore.conf
[ -f "$CONF" ] && . "$CONF"
export S3_ENDPOINT S3_REGION S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY
export SSL_CERT_DIR=/system/etc/security/cacerts
export DIASPORE_DNS="${DIASPORE_DNS:-1.1.1.1:53}"
# Restore progress: the agent writes "<phase> <done> <total>" here per chunk; the login daemon polls it
# and streams it to the gate's restore bar. Lives in the RAM tmpfs (gone on power-off, never roams).
export DIASPORE_PROGRESS="$STATE/roam.progress"

# The agent untars as root, so restored data ends up root:root -- the app, running as its per-user uid,
# then can't open it and crash-loops (launcher icon cache, media/contacts providers, SQLiteCantOpen, ...).
# These helpers re-own restored data to the uid Android already assigned to the (untouched) top-level dir
# it created, so we never hardcode a uid (works for any user id / appId / media domain).

# remap_per_pkg DIR: each immediate child of DIR is a package dir Android created and owns; re-own its
# contents to that uid. Leave cache/code_cache alone (special per-user cache GID we must not clobber).
# Used for CE (/data/user/N) and DE (/data/user_de/N) app storage.
remap_per_pkg() {
  for pkg in "$1"/*; do
    [ -d "$pkg" ] || continue
    own=$(stat -c '%u' "$pkg" 2>/dev/null)
    case "$own" in ''|*[!0-9]*) continue;; esac
    [ "$own" -ge 100000 ] || continue       # a real per-user app uid (userId*100000+appId); skip root-owned orphans
    for sub in "$pkg"/*; do
      case "${sub##*/}" in cache|code_cache) continue;; esac
      [ -e "$sub" ] && chown -R "$own:$own" "$sub" 2>> "$LOG"
    done
  done
}

# remap_tree DIR: the shared media tree is one ownership domain (media_rw), so re-own the whole restored
# tree to the uid:gid Android gave DIR itself. Used for /data/media/N (photos, downloads, files).
remap_tree() {
  own=$(stat -c '%u:%g' "$1" 2>/dev/null)
  case "$own" in ''|*[!0-9:]*) return;; esac
  chown -R "$own" "$1" 2>> "$LOG"
}

# relabel_launcher UID USERDIR: Launcher3 keys every home/dock item to the user's SERIAL number
# (favorites.profileId). An ephemeral roamed user gets a NEW serial each login, so a restored launcher.db
# carries a STALE serial -> Launcher3 finds no matching profile and DROPS every item == the home-screen
# layout never roams (while app DATA does, since data isn't serial-keyed). Rewrite profileId to THIS user's
# serial (from dumpsys -- the /data/system/users xml is binary ABX, not greppable) so the home survives.
# Run BEFORE remap_per_pkg/restorecon so they re-own + relabel anything sqlite touches.
relabel_launcher() {
  ldb="$2/com.android.launcher3/databases/launcher.db"
  [ -f "$ldb" ] || return
  serial=$(dumpsys user 2>/dev/null | grep "UserInfo{$1:" | grep -oE 'serialNo=[0-9]+' | head -1 | cut -d= -f2)
  [ -n "$serial" ] || { echo "[roamd] launcher relabel: no serial for user $1" >> "$LOG"; return; }
  if /system/bin/sqlite3 "$ldb" "UPDATE favorites SET profileId=$serial;" >> "$LOG" 2>&1; then
    echo "[roamd] relabeled launcher favorites -> serial $serial (user $1)" >> "$LOG"
  else
    echo "[roamd] launcher relabel FAILED (user $1)" >> "$LOG"
  fi
}

[ -f "$REQ" ] || { echo "ERR-NOREQ" > "$RES" 2>/dev/null; exit 0; }
op=$(sed -n 1p "$REQ"); name=$(sed -n 2p "$REQ"); pass=$(sed -n 3p "$REQ"); uid=$(sed -n 4p "$REQ")
rm -f "$REQ"                           # consume creds immediately -- RAM-only, sub-second window

# Live timezone apply (DIA-20260616-60). The in-session picker can't set the GLOBAL system zone (a secondary-user
# app), and the confined daemon can't either -- only root can. zone is passed in the NAME slot (empty = Automatic);
# cmd needs /dev/null stdio from su:s0 (the cmd fd-passing denial, DIA-56). Handled BEFORE the name/uid check (a
# settz has no name/uid). Applied here so the pick takes effect immediately, not just on the next login.
if [ "$op" = "settz" ]; then
  zone="$name"
  if [ -n "$zone" ]; then
    settings put global auto_time_zone 0 </dev/null >/dev/null 2>&1   # OVERRIDE: detector off so the pin sticks
    cmd alarm set-timezone "$zone" </dev/null >/dev/null 2>&1
    echo "[roamd] settz override $zone $(date)" >> "$LOG"
  else
    def=$(getprop ro.diaspore.default.timezone)                       # AUTOMATIC: seed the default, then auto on
    [ -n "$def" ] && cmd alarm set-timezone "$def" </dev/null >/dev/null 2>&1
    settings put global auto_time_zone 1 </dev/null >/dev/null 2>&1
    echo "[roamd] settz automatic (seed $def) $(date)" >> "$LOG"
  fi
  echo "OK" > "$RES"; chmod 600 "$RES"; exit 0
fi

[ -n "$op" ] && [ -n "$name" ] && [ -n "$uid" ] || { echo "ERR-ARGS" > "$RES"; chmod 600 "$RES"; exit 0; }
# uid builds a root path (USERDIR) below -- defense-in-depth: reject anything non-numeric (DIA-20260618-08, audit #3).
case "$uid" in *[!0-9]*) echo "[roamd] non-numeric uid rejected $(date)" >> "$LOG"; echo "ERR-UID" > "$RES"; chmod 600 "$RES"; exit 0;; esac
USERDIR="/data/user/$uid"

case "$op" in
  in)
    # CE storage of a RUNNING user is unlocked (the chooser started user N before triggering us). The CE
    # restore is the login GATE -- if its ref doesn't resolve (wrong creds / no profile) the user lands
    # empty == blind login; DE + media below are best-effort completeness on top of a valid CE login.
    if DIASPORE_PHASE=apps "$AGENT" restore s3 "$name" "$pass" "$USERDIR" >> "$LOG" 2>&1; then
      relabel_launcher "$uid" "$USERDIR"   # rewrite home-layout serial so the launcher keeps it (see above)
      remap_per_pkg "$USERDIR"
      restorecon -R "$USERDIR" 2>> "$LOG"
      # Roam completeness: also restore device-encrypted per-user app data and the shared media tree
      # (photos, downloads, files), each on its own store ref (name#de / name#media) so a fresh profile
      # simply has none yet and the restore is a harmless no-op. Best-effort: a failure here does not
      # un-gate the (already valid) CE login.
      DEDIR="/data/user_de/$uid"
      if [ -d "$DEDIR" ] && DIASPORE_PHASE=secure "$AGENT" restore s3 "${name}#de" "$pass" "$DEDIR" >> "$LOG" 2>&1; then
        remap_per_pkg "$DEDIR"; restorecon -R "$DEDIR" 2>> "$LOG"
      fi
      MEDIADIR="/data/media/$uid"
      if [ -d "$MEDIADIR" ] && DIASPORE_PHASE=media "$AGENT" restore s3 "${name}#media" "$pass" "$MEDIADIR" >> "$LOG" 2>&1; then
        remap_tree "$MEDIADIR"; restorecon -R "$MEDIADIR" 2>> "$LOG"
      fi
      # App provisioning: surface the roamed launchable-app list (the chooser sealed it into its own files
      # dir with the data) into the RAM tmpfs, so the daemon can hand it back to the chooser to reinstall
      # the apps for the fresh user (the worker can't do pm under the device owner).
      : > "$STATE/apps.out"
      [ -f "$USERDIR/com.diaspore.chooser/files/diaspore-apps.list" ] \
        && cp "$USERDIR/com.diaspore.chooser/files/diaspore-apps.list" "$STATE/apps.out" 2>> "$LOG"
      # Roamed prefs (timezone + locale): same surface-from-the-sealed-CE-data trick as the app list, so the
      # user-0 gate can re-apply them on login (the worker can't set tz/locale; the device-owner gate does).
      : > "$STATE/prefs.out"
      [ -f "$USERDIR/com.diaspore.chooser/files/diaspore-prefs" ] \
        && cp "$USERDIR/com.diaspore.chooser/files/diaspore-prefs" "$STATE/prefs.out" 2>> "$LOG"
      echo "[roamd] IN $name -> user $uid (+de +media) $(date)" >> "$LOG"
      echo "OK $uid" > "$RES"
    else
      # Wrong creds / no profile -> unguessable ref resolves to nothing == blind login (empty user).
      echo "[roamd] IN $name BLANK (bad creds / no profile) $(date)" >> "$LOG"
      echo "BLANK" > "$RES"
    fi
    ;;
  out)
    # Seal while the user is still RUNNING (CE unlocked); the chooser stops+removes it after we return.
    # Seal all three classes (CE + DE + shared media), each to its own ref, mirroring the restore split.
    rc=0
    "$AGENT" push s3 "$name" "$pass" "$USERDIR" >> "$LOG" 2>&1 || rc=1
    [ -d "/data/user_de/$uid" ] && { "$AGENT" push s3 "${name}#de"    "$pass" "/data/user_de/$uid" >> "$LOG" 2>&1 || rc=1; }
    [ -d "/data/media/$uid" ]   && { "$AGENT" push s3 "${name}#media" "$pass" "/data/media/$uid"   >> "$LOG" 2>&1 || rc=1; }
    if [ "$rc" = 0 ]; then
      echo "[roamd] OUT $name (user $uid) +de +media $(date)" >> "$LOG"
      echo "OK" > "$RES"
    else
      echo "[roamd] OUT $name ERR-PUSH $(date)" >> "$LOG"
      echo "ERR-PUSH" > "$RES"
    fi
    ;;
  *) echo "ERR-OP" > "$RES" ;;
esac
chmod 600 "$RES"
