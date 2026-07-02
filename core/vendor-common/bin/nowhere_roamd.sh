#!/system/bin/sh
# Nowhere Arc 2 per-user DATA worker (su:s0). The chooser (device owner) owns the user LIFECYCLE
# (create/start/switch/stop/remove via DPM/AM -- M1); THIS does only the privileged DATA half the
# confined nowhere domain can't: restore arbitrary app data into /data/user/N + restorecon, and seal it
# back. On-demand -- an app can't spawn su:s0, so the caller writes the request file into the RAM tmpfs
# then `setprop ctl.start nowhere_roamd`; init runs this once; it writes the result file.
#   req (state tmpfs = RAM, consumed immediately):  op\nname\npass\nuid
#   res (state tmpfs = RAM):                         OK <uid> | BLANK | ERR-...
# req/res live in /data/nowhere/state (the tmpfs, gone on power-off; the sync skips plain files, so the
# creds never roam) -- same trick as the Arc-1 .session marker.
STATE=/data/nowhere/state
REQ="$STATE/roam.req"
RES="$STATE/roam.res"
LOG=/data/nowhere/roam.log            # status only -- never creds
AGENT=/system/bin/nowhere_agent
# Store config: prefer the provisioned, mutable /data copy (out of the read-only image + rotatable);
# fall back to a baked /system copy for older / un-migrated devices.
CONF=/data/nowhere/nowhere.conf
[ -f "$CONF" ] || CONF=/system/etc/nowhere/nowhere.conf
[ -f "$CONF" ] && . "$CONF"
export S3_ENDPOINT S3_REGION S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY
# Managed/cap mode (Slice B / cap-gated I/O): the seal worker now PAYS -- capFlush leases the new chunks it
# writes -- so it needs the billing gateway and the session wallet (login daemon restores it to $STATE).
export GATEWAY_URL NOWHERE_STATE="$STATE"
# capFlush spills sealed blob bytes here while STREAMING them to the store (#45), so a multi-GB seal can't
# OOM the device. MUST be on DISK -- NOT the tmpfs $STATE -- and is cleared per flush. /data has the space.
export NOWHERE_SPILL=/data/nowhere/spill
mkdir -p "$NOWHERE_SPILL"
export SSL_CERT_DIR=/system/etc/security/cacerts
# The agent auto-discovers the device's own per-network DNS (privacy: no public-resolver leak), with a
# public fallback only if discovery fails. Set NOWHERE_DNS=host[:port] to force a specific resolver.
export NOWHERE_DNS="${NOWHERE_DNS:-}"
# Restore progress: the agent writes "<phase> <done> <total>" here per chunk; the login daemon polls it
# and streams it to the gate's restore bar. Lives in the RAM tmpfs (gone on power-off, never roams).
export NOWHERE_PROGRESS="$STATE/roam.progress"
# Known-present blob cache (DIA-20260625-08): the agent records confirmed-in-store chunk hashes here (RAM,
# per-store file) so an unchanged re-seal skips the per-chunk network stat -> a no-change logoff finishes in
# ~1-2s instead of 30-60s. RAM dir, so it's re-warmed by the first periodic seal after each boot.
export NOWHERE_BLOBCACHE="$STATE"

# The agent untars as root, so restored data ends up root:root -- the app, running as its per-user uid,
# then can't open it and crash-loops (launcher icon cache, media/contacts providers, SQLiteCantOpen, ...).
# These helpers re-own restored data to the uid Android already assigned to the (untouched) top-level dir
# it created, so we never hardcode a uid (works for any user id / appId / media domain).

# remap_per_pkg DIR: each immediate child of DIR is a package dir Android created and owns; re-own its
# contents to that uid. Leave cache/code_cache alone (special per-user cache GID we must not clobber).
# Used for CE (/data/user/N) and DE (/data/user_de/N) app storage.
remap_per_pkg() {
  # local-scope EVERYTHING: without this, `name=` below clobbers the GLOBAL $name (the login profile), and
  # since this runs BETWEEN the CE restore and the DE/media restores, those then resolve "<lastpkg>#de" instead
  # of "<profile>#de" -> DE + media SILENTLY don't restore (regression since DIA-20260625-03 added the reown
  # branch; surfaced by the #72 receipt guard refusing to seal the un-restored heads). su:s0 sh is mksh -> local ok.
  local pkg own name appid target sub
  for pkg in "$1"/*; do
    [ -d "$pkg" ] || continue
    own=$(stat -c '%u' "$pkg" 2>/dev/null)
    case "$own" in ''|*[!0-9]*) continue;; esac
    if [ "$own" -lt 100000 ]; then
      # ROOT-OWNED (0:0) means the RESTORE created this app dir -- the app isn't install-existing'd for this
      # user yet (provisionRoamedApps runs AFTER us). If we leave it root-owned, install-existing's installd
      # finds the wrong owner ("Expected <uid> but found 0:0"), fails, and PackageManager "recovers" by WIPING
      # the dir -- destroying the just-restored app data (e.g. downloaded maps; every roamed third-party app
      # came back empty). So give it the CORRECT per-user uid now (userId*100000 + appId), so install-existing
      # REUSES the dir instead of recreating it. appId = field 2 of packages.list -- a plain file read, since
      # `pm`/`cmd` from su:s0 can't capture output (DIA-56). restorecon (run right after us) fixes the labels.
      # (DIA-20260625-03; the old `>= 100000` guard skipped these dirs, which is exactly what lost the data.)
      name="${pkg##*/}"
      appid=$(grep -m1 "^$name " /data/system/packages.list 2>/dev/null | cut -d' ' -f2)
      case "$appid" in ''|*[!0-9]*) continue;; esac
      target=$((uid * 100000 + appid))
      chown -R "$target:$target" "$pkg" 2>> "$LOG"
      echo "[roamd] reowned restored $name -> $target (user $uid)" >> "$LOG"
      continue
    fi
    for sub in "$pkg"/*; do
      case "${sub##*/}" in cache|code_cache) continue;; esac
      [ -e "$sub" ] && chown -R "$own:$own" "$sub" 2>> "$LOG"
    done
  done
}

# remap_tree DIR: the shared media tree is one ownership domain (media_rw), so re-own the whole restored
# tree to the uid:gid Android gave DIR itself. Used for /data/media/N (photos, downloads, files).
remap_tree() {
  local own
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
  local ldb serial
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
sesec=$(sed -n 5p "$REQ")              # line 5 (E.3b): se_secret hex for a hardened Endospore identity; empty otherwise
[ -n "$sesec" ] && export NOWHERE_SE_SECRET="$sesec"  # -> agent opens/seals via the `se` keyslot (docs/se-binding.md)
skind=$(sed -n 6p "$REQ")              # line 6 (#58): seal kind -- "manual" for a Back up now, empty for auto (sync/logoff)
[ -n "$skind" ] && export NOWHERE_SEAL_KIND="$skind"  # -> the seal tags its snapshot manual (pinned) vs automatic
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
    def=$(getprop ro.nowhere.default.timezone)                       # AUTOMATIC: seed the default, then auto on
    [ -n "$def" ] && cmd alarm set-timezone "$def" </dev/null >/dev/null 2>&1
    settings put global auto_time_zone 1 </dev/null >/dev/null 2>&1
    echo "[roamd] settz automatic (seed $def) $(date)" >> "$LOG"
  fi
  echo "OK" > "$RES"; chmod 600 "$RES"; exit 0
fi

# Cold-lock cache flush (P3, DIA-20260625-13): after the chooser STOPs a user (CE key evicted), drop the kernel
# dentry/inode cache so even a POWERED-ON locked device is immediately ciphertext -- a stopped user's already-
# cached inodes stay readable from RAM until dropped. Root-only; no name/uid, so handled before the arg check.
if [ "$op" = "dropcaches" ]; then
  sync
  echo 3 > /proc/sys/vm/drop_caches 2>> "$LOG"
  echo "[roamd] drop_caches $(date)" >> "$LOG"
  echo "OK" > "$RES"; chmod 600 "$RES"; exit 0
fi

# Cold-lock RESUME credential check (P4, DIA-20260625-13): verify the freshly typed passphrase against a
# COLD-LOCKED user's CE so the gate can resume it (verify-BEFORE-switch is what avoids the keyguard-timeout
# crash when switching into a stopped user). cmd from su:s0 must use /dev/null stdio (the DIA-56 fd-passing
# denial); we therefore do NOT capture cmd's output -- instead we read the GROUND TRUTH: a locked user's
# /data dir holds only ENCRYPTED filenames, so `/data/user/<uid>/android` resolves IFF the verify actually
# decrypted the CE. (The caller drop_caches'd at cold-lock, so a stale RAM cache can't fake the unlocked
# state.) uses uid but no name/profile, so handled before the name/uid arg check. The pass is NEVER logged.
if [ "$op" = "verify" ]; then
  case "$uid" in ''|*[!0-9]*) echo "ERR-UID" > "$RES"; chmod 600 "$RES"; exit 0;; esac
  cmd lock_settings verify --old "$pass" --user "$uid" </dev/null >/dev/null 2>/dev/null
  if ls -d "/data/user/$uid/android" >/dev/null 2>&1; then
    # CE decrypted. Now REMOVE the lockscreen credential so the resume's switch lands straight on HOME with a
    # SINGLE passphrase, instead of re-prompting on user N's own Android keyguard (the double-prompt). The chooser
    # re-arms the credential on the next screen-off, exactly like a fresh login (which also lands on home, no
    # lockscreen, and arms on first screen-off). `clear` needs the CE unlocked, which the verify above just did.
    cmd lock_settings clear --old "$pass" --user "$uid" </dev/null >/dev/null 2>/dev/null
    echo "OK" > "$RES"; echo "[roamd] resume verify+clear user $uid OK $(date)" >> "$LOG"
  else
    echo "FAIL" > "$RES"; echo "[roamd] resume verify user $uid FAIL $(date)" >> "$LOG"
  fi
  chmod 600 "$RES"; exit 0
fi

# Orphan per-user storage cleanup (P4, DIA-20260625-13): removeUser leaves /data/system_ce/<uid> (+ misc_ce) on
# this FBE build, so a REUSED user-id inherits the stale, key-mismatched CE and crashes system_server on unlock
# (AccountManagerService accounts_ce.db, code 14 SQLITE_CANTOPEN). The chooser calls this AFTER removeUser -- the
# uid is gone, so there's no live user to race -- to rm the orphaned per-uid dirs; the system recreates fresh ones
# for the next user with that id. HARD GUARD: only secondary users (uid >= 10) -- never touch user 0's storage.
if [ "$op" = "cleanstorage" ]; then
  case "$uid" in ''|*[!0-9]*) echo "ERR-UID" > "$RES"; chmod 600 "$RES"; exit 0;; esac
  if [ "$uid" -lt 10 ]; then echo "[roamd] cleanstorage REJECT uid $uid (<10) $(date)" >> "$LOG"; echo "ERR-UID" > "$RES"; chmod 600 "$RES"; exit 0; fi
  for d in system_ce system_de misc_ce misc_de user user_de media vendor_ce vendor_de; do
    rm -rf "/data/$d/$uid" 2>> "$LOG"
  done
  echo "[roamd] cleanstorage user $uid $(date)" >> "$LOG"
  echo "OK" > "$RES"; chmod 600 "$RES"; exit 0
fi

# Manual-lock self-arm (P4, DIA-20260625-13): a manual cold-lock right after login may have NO lockscreen credential
# yet (the cred-armer normally sets it on the FIRST screen-off). So set it now = the session passphrase, so the
# cold-lock is passphrase-protected + RESUMABLE with zero user action. Best-effort: if a credential is already set
# (the idle path, screen-off already armed it) set-password WITHOUT --old fails harmlessly, leaving the existing
# (same) credential. Root; /dev/null stdio (the DIA-56 cmd fd denial). The pass is NEVER logged.
if [ "$op" = "armcred" ]; then
  case "$uid" in ''|*[!0-9]*) echo "ERR-UID" > "$RES"; chmod 600 "$RES"; exit 0;; esac
  cmd lock_settings set-password --user "$uid" "$pass" </dev/null >/dev/null 2>/dev/null || true
  echo "[roamd] armcred user $uid $(date)" >> "$LOG"
  echo "OK" > "$RES"; chmod 600 "$RES"; exit 0
fi

[ -n "$op" ] && [ -n "$name" ] && [ -n "$uid" ] || { echo "ERR-ARGS" > "$RES"; chmod 600 "$RES"; exit 0; }
# uid builds a root path (USERDIR) below -- defense-in-depth: reject anything non-numeric (DIA-20260618-08, audit #3).
case "$uid" in *[!0-9]*) echo "[roamd] non-numeric uid rejected $(date)" >> "$LOG"; echo "ERR-UID" > "$RES"; chmod 600 "$RES"; exit 0;; esac
USERDIR="/data/user/$uid"

case "$op" in
  in)
    # #72: start each login with NO restore-completion receipts, so a receipt certifies only what THIS session
    # actually restores (each phase's `restore` writes its own on full success). A phase whose restore fails
    # leaves no receipt -> pushProfile refuses to seal that ref over its good head. RAM tmpfs, per-boot.
    rm -rf "$STATE/restore-receipts" 2>> "$LOG"
    # CE storage of a RUNNING user is unlocked (the chooser started user N before triggering us). The CE
    # restore is the login GATE -- if its ref doesn't resolve (wrong creds / no profile) the user lands
    # empty == blind login; DE + media below are best-effort completeness on top of a valid CE login.
    if NOWHERE_PHASE=apps "$AGENT" restore s3 "$name" "$pass" "$USERDIR" >> "$LOG" 2>&1; then
      relabel_launcher "$uid" "$USERDIR"   # rewrite home-layout serial so the launcher keeps it (see above)
      remap_per_pkg "$USERDIR"
      restorecon -R "$USERDIR" 2>> "$LOG"
      # Roam completeness: also restore device-encrypted per-user app data and the shared media tree
      # (photos, downloads, files), each on its own store ref (name#de / name#media) so a fresh profile
      # simply has none yet and the restore is a harmless no-op. Best-effort: a failure here does not
      # un-gate the (already valid) CE login.
      DEDIR="/data/user_de/$uid"
      if [ -d "$DEDIR" ] && NOWHERE_PHASE=secure "$AGENT" restore s3 "${name}#de" "$pass" "$DEDIR" >> "$LOG" 2>&1; then
        remap_per_pkg "$DEDIR"; restorecon -R "$DEDIR" 2>> "$LOG"
      fi
      MEDIADIR="/data/media/$uid"
      if [ -d "$MEDIADIR" ] && NOWHERE_PHASE=media "$AGENT" restore s3 "${name}#media" "$pass" "$MEDIADIR" >> "$LOG" 2>&1; then
        remap_tree "$MEDIADIR"; restorecon -R "$MEDIADIR" 2>> "$LOG"
      fi
      # App provisioning: surface the roamed launchable-app list (the chooser sealed it into its own files
      # dir with the data) into the RAM tmpfs, so the daemon can hand it back to the chooser to reinstall
      # the apps for the fresh user (the worker can't do pm under the device owner).
      : > "$STATE/apps.out"
      [ -f "$USERDIR/com.nowhere.chooser/files/nowhere-apps.list" ] \
        && cp "$USERDIR/com.nowhere.chooser/files/nowhere-apps.list" "$STATE/apps.out" 2>> "$LOG"
      # Roamed prefs (timezone + locale): same surface-from-the-sealed-CE-data trick as the app list, so the
      # user-0 gate can re-apply them on login (the worker can't set tz/locale; the device-owner gate does).
      : > "$STATE/prefs.out"
      [ -f "$USERDIR/com.nowhere.chooser/files/nowhere-prefs" ] \
        && cp "$USERDIR/com.nowhere.chooser/files/nowhere-prefs" "$STATE/prefs.out" 2>> "$LOG"
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
    # (Runtime perms are captured by the chooser at logoff into files/nowhere-perms -- via PackageManager, which
    # reads LIVE state; the on-disk runtime-permissions.xml is written lazily so a su:s0 read here is stale.)
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
  grant)
    # Re-grant the roamed runtime permissions captured at the last logout, now that the chooser has
    # install-existing'd the apps for this user (a perm can only be granted to an INSTALLED app, so this runs
    # AFTER provisionRoamedApps, triggered by the chooser). pm from su:s0 works with /dev/null stdio -- the cmd
    # fd-passing denial (DIA-56) is only about CAPTURING output, which we don't. (DIA-20260625-06)
    PF="$USERDIR/com.nowhere.chooser/files/nowhere-perms"
    n=0
    if [ -f "$PF" ]; then
      while read -r gpkg gperm; do
        [ -n "$gpkg" ] && [ -n "$gperm" ] || continue
        pm grant --user "$uid" "$gpkg" "$gperm" </dev/null >/dev/null 2>&1 && n=$((n+1))
      done < "$PF"
    fi
    echo "[roamd] GRANT -> $n perms (user $uid) $(date)" >> "$LOG"
    echo "OK" > "$RES"
    ;;
  *) echo "ERR-OP" > "$RES" ;;
esac
chmod 600 "$RES"
