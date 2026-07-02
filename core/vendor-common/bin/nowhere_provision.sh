#!/system/bin/sh
# Nowhere first-boot provisioning: make the chooser the DEVICE OWNER so it can drive the kiosk Lock
# Task on the gate -- with NO manual adb. Runs once per fresh /data (a marker, cleared by a factory
# reset). su:s0 (not the confined nowhere domain) because dpm/settings need broad framework access.
LOG=/data/nowhere/provision.log
MARK=/data/nowhere/.provisioned
ADMIN=com.nowhere.chooser/.AdminReceiver
mkdir -p /data/nowhere

# DEV (userdebug) ONLY: enable `adb root` durably. LineageOS gates `adb root` behind the IADBRootService,
# whose enabled-state is the file /data/adbroot/enabled -- normally written only by the Settings "Rooted
# debugging" toggle (system uid), which is UNREACHABLE on this product because the kiosk hides Settings. We
# already run as root on every boot (above the run-once marker), so seed the file + relabel + bounce the
# service. Userdebug-only, so production `user` builds are untouched. (DIA-20260625-02 -- on-device debugging.)
if [ "$(getprop ro.build.type)" = "userdebug" ]; then
  mkdir -p /data/adbroot
  echo 1 > /data/adbroot/enabled
  restorecon -R /data/adbroot 2>/dev/null
  setprop ctl.restart adb_root
fi

[ -f "$MARK" ] && exit 0

# boot_completed can still race the package scan on a very first boot; wait for the chooser to exist.
i=0
while [ "$i" -lt 10 ]; do
  pm path com.nowhere.chooser >/dev/null 2>&1 && break
  i=$((i + 1)); sleep 2
done

# Is the chooser the DEVICE OWNER? Read the state file system_server persists, do NOT parse `dpm list-owners`.
# WHY (the bug that made a freshly-wiped device stay a NON-kiosk gate forever): a `cmd` invocation -- which is
# what dpm/pm/am/settings all are -- hands ITS stdio fds to system_server over the binder transaction so the
# service can write the reply. From this su:s0 init service those fds point at our LOG (nowhere_data_file) or,
# for `$(...)`/pipes, an su:s0 fifo -- and system_server is DENIED use of both under enforcing sepolicy, so the
# whole transaction aborts ("Failure calling service ...: Failed transaction (2147483646)") and the command
# NEVER RUNS. (The same call from `adb shell` works: shell's pty fds are ones system_server services freely --
# which is why a manual `dpm set-device-owner` always fixed it.) So: every cmd below sends stdio to /dev/null
# (a null_device fd system_server CAN use -> the call actually executes), and we read success from the file
# system_server already wrote in its own context (no fd hand-off). device_policies.xml lists the *active admin*
# even without ownership -> would false-positive; only device_owner*.xml means DEVICE OWNER.
owner_ok() {
  for f in /data/system/device_owner_2.xml /data/system/device_owner.xml; do
    [ -f "$f" ] && grep -q "com.nowhere.chooser" "$f" 2>/dev/null && return 0
  done
  return 1
}

# A FOREIGN device-owner -- a DO record present that is NOT us (e.g. a pre-rename com.diaspore.chooser left in
# /data by a system-only reflash) -- BLOCKS set-device-owner (one DO is allowed, no cross-package transfer)
# and can't be cleared from outside the owning app. So a package RENAME on a provisioned device is a hard
# factory-reset migration (docs/rename-migration.md). Only consulted when owner_ok is already false, so any DO
# record found here is foreign. Detect it to log a clear signal instead of burning the retry budget on a
# doomed set + leaving a silent non-kiosk gate.
foreign_owner() {
  for f in /data/system/device_owner_2.xml /data/system/device_owner.xml; do
    [ -f "$f" ] && grep -q 'package=' "$f" 2>/dev/null && return 0
  done
  return 1
}

# Un-stop the chooser so its DeviceAdminReceiver can register/enable. A freshly-flashed system app is in the
# Android-12+ STOPPED state, and its admin can't be activated until the app has been launched once.
am start -n com.nowhere.chooser/.ChooserActivity </dev/null >/dev/null 2>&1

# Claim DEVICE OWNERSHIP. At boot_completed the device is briefly not ready in several transient ways (admin
# not registered with DPMS yet; a transient boot-time account; services still overloaded), all of which clear
# once it settles. The old code blamed THIS for the failures and added a 180s "readiness probe" + retries that
# STILL failed -- because the real cause was the fd-passing denial above (the probe's `dumpsys account | grep`
# went through an su:s0 pipe and never succeeded; the set attempt's `$(...)` capture aborted the transaction).
# With stdio fixed to /dev/null the call actually executes, and a plain retry loop subsumes the readiness wait:
# keep trying (set-active-admin re-registers the admin each pass; device_provisioned toggled 0 around the set,
# as set-device-owner requires) until ownership STICKS in the file, or a generous cap. We mark done (far below)
# ONLY once owner_ok, so a still-not-ready first boot re-attempts next boot instead of stranding a non-kiosk.
attempt=0
if owner_ok; then
  : # already owned by us
elif foreign_owner; then
  # A different package owns the device (almost always a pre-rename com.diaspore.chooser left in /data by a
  # system-only reflash). set-device-owner cannot override it and it can't be cleared from here, so retrying
  # is pointless -- the device needs a FACTORY RESET to re-provision (user data is safe; it roams from the
  # store). See docs/rename-migration.md. Logged every boot (no marker is written) until the reset.
  echo "[provision] FOREIGN device-owner present (not com.nowhere.chooser) -- a package rename can't transfer ownership; a FACTORY RESET is required to re-provision (docs/rename-migration.md). Not retrying. $(date)" >> "$LOG"
else
  while ! owner_ok && [ "$attempt" -lt 60 ]; do
    settings put global device_provisioned 0 </dev/null >/dev/null 2>&1
    settings put secure user_setup_complete 0 </dev/null >/dev/null 2>&1
    dpm set-active-admin --user 0 "$ADMIN" </dev/null >/dev/null 2>&1
    dpm set-device-owner "$ADMIN" </dev/null >/dev/null 2>&1
    settings put global device_provisioned 1 </dev/null >/dev/null 2>&1
    settings put secure user_setup_complete 1 </dev/null >/dev/null 2>&1
    owner_ok && break
    attempt=$((attempt + 1))
    sleep 3
  done
fi
if owner_ok; then
  echo "[provision] device-owner CONFIRMED after $attempt attempts $(date)" >> "$LOG"
elif ! foreign_owner; then
  echo "[provision] device-owner NOT set after $attempt attempts -- will retry next boot $(date)" >> "$LOG"
fi

# Disable the redundant keyguard/lockscreen. The blind-login gate IS the lock (there is no PIN), so the
# default swipe keyguard only flashes for ~1s before the gate kiosks at boot. Done here (su:s0, once per
# fresh /data) so it's DURABLE -- the equivalent runtime `locksettings set-disabled true` is wiped by a
# factory reset / unlock. (/dev/null stdio: same fd-passing reason as above.)
locksettings set-disabled true </dev/null >/dev/null 2>&1
echo "[provision] keyguard disabled (locksettings set-disabled true) $(date)" >> "$LOG"

# Dev convenience (DIA-20260623-25): on a DEBUGGABLE build, keep ADB usable across the /data wipe. A fresh
# /data defaults USB-debugging OFF, and the device-owner kiosk gate blocks Developer options -- so a wiped
# dev device strands the adb loop with no in-OS way back. Guarded to ro.debuggable=1, so a user/production
# build NEVER does this. (/dev/null stdio: the cmd fd-passing reason as above, DIA-20260616-56.)
if [ "$(getprop ro.debuggable)" = "1" ]; then
  settings put global development_settings_enabled 1 </dev/null >/dev/null 2>&1
  settings put global adb_enabled 1 </dev/null >/dev/null 2>&1
  echo "[provision] (dev/debuggable) adb_enabled set so it survives the wipe $(date)" >> "$LOG"
fi

# Disable the LineageOS SetupWizard on user 0. On a provisioned Nowhere device the blind-login gate IS the
# setup -- there's no wizard step -- but SetupWizard can WIN the boot foreground race and sit in front of the
# gate ("welcome screen"); that also blocks the gate's device-owner Lock Task from engaging (startLockTask
# needs the FOREGROUND task -> "Invalid task, not in foreground" -> the kiosk never locks). Disable it here
# (su:s0, once per fresh /data; the disabled state persists until a factory reset, which re-runs provisioning)
# so it can never win that race. (prepRoamedUser already disables it for the roamed users; this covers user 0.)
pm disable-user --user 0 org.lineageos.setupwizard </dev/null >/dev/null 2>&1
echo "[provision] setupwizard disabled on user 0 $(date)" >> "$LOG"

# Default geolocation time-zone detection ON for all users (DIA-20260617-01). The AOSP offline geo tz provider
# is integrated (DIA-20260616-61) and the master switch is on, but the per-user "use location for time zone"
# setting defaults OFF (ServerFlags KEY_LOCATION_TIME_ZONE_DETECTION_SETTING_ENABLED_DEFAULT) -- so geo only
# runs if a user toggles it. Flip the global default on so every fresh (ephemeral) roamed user auto-uses geo in
# Automatic mode (no SIM -> NITZ is silent, so geo is the live source). It's a GLOBAL DeviceConfig flag
# (system_time namespace), so all users inherit it with no per-user write; no GApps -> no server sync to
# overwrite it. cmd needs /dev/null stdio from su:s0 (the cmd fd-passing denial, DIA-20260616-56).
device_config put system_time location_time_zone_detection_setting_enabled_default true </dev/null >/dev/null 2>&1
echo "[provision] geolocation tz detection defaulted ON $(date)" >> "$LOG"

# Arm the gate on a cold device. A freshly-flashed system app is left in the STOPPED state (Android 12+),
# and stopped apps do NOT receive BOOT_COMPLETED -- so the chooser's BootReceiver never fires and the gate
# never auto-arms on the very first boots after a wipe. Launching the activity once here clears the stopped
# flag (the un-stopped state persists in /data across reboots), so every subsequent boot the BootReceiver
# arms the gate normally. Re-launching after ownership is set also re-triggers the gate's onResume Lock Task
# now that it can actually lock. su:s0 can start it; the chooser kiosk-locks itself in onResume.
am start -n com.nowhere.chooser/.ChooserActivity </dev/null >/dev/null 2>&1
echo "[provision] launched gate to clear stopped-state $(date)" >> "$LOG"

# Mark done ONLY once device-owner is actually set, so a first boot where the device-policy service wasn't
# ready re-attempts on the NEXT boot instead of leaving the device stuck as a non-kiosk gate forever. The
# steps above (keyguard / setupwizard-disable / gate-launch) are idempotent, so re-running them is harmless.
if owner_ok; then
  touch "$MARK"
  echo "[provision] done; marker written $(date)" >> "$LOG"
else
  echo "[provision] marker NOT written -- provisioning will re-run next boot $(date)" >> "$LOG"
fi
