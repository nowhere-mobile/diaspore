# Diaspore system-side components (P4.2).
# Inherited from device/fairphone/FP3/lineage_FP3.mk via inherit-product-if-exists.
PRODUCT_PACKAGES += \
    nowhere_agent \
    nowhere_login.sh \
    nowhere_provision.sh \
    nowhere_roamd.sh \
    nowhere_otad.sh \
    nowhere_gate.sh \
    nowhere.rc \
    NowhereChooser \
    FDroid

# Dev-only adb pre-authorization (DIA-20260623-25) -- a DEAD END on this FP3 with our system-only flash, so
# it's DEFERRED (DIA-20260624-04). The device-owner kiosk SUPPRESSES the "Allow USB debugging?" RSA dialog, so
# adb stays `unauthorized` with no interactive way to approve it. Every auto-trust route fails when we flash
# only system+vbmeta (`p4.5-sysimg.sh`):
#  - PRODUCT_ADB_KEYS (keyed): bakes the key to /adb_keys, but the FP3 is recovery-as-boot so it lands in the
#    BOOT ramdisk, which a system flash never touches. Kept below -- effective only on a full/boot flash.
#  - ro.adb.secure=0 (disable auth): OVERRIDDEN -- vendor/lineage/config/common.mk forces ro.adb.secure=1 for
#    userdebug AND device/fairphone/FP3 init.qcom.usb.rc does `setprop ro.adb.secure 1` at runtime, both beating
#    PRODUCT_PROPERTY_OVERRIDES, so system/build.prop still ships =1. (So that override is intentionally NOT set.)
# A real fix needs a device-tree / boot change (init.qcom.usb.rc + the boot ramdisk), out of this repo and not
# carried by a system flash. Until then the dev loop flashes via a manual bootloader entry.
# PROVISION (gitignored, per-builder) for the keyed bake on a FULL flash: copy your ~/.android/adbkey.pub to
# core/vendor-common/dev-adb_keys; stage-vendor stages it into vendor/diaspore/dev-adb_keys.
ifneq (,$(filter eng userdebug,$(TARGET_BUILD_VARIANT)))
PRODUCT_ADB_KEYS := vendor/diaspore/dev-adb_keys
endif

# Curated default apps (DIA-20260617-06): a small, privacy-first, NO-GApps F-Droid set so the device is
# useful out of the box (F-Droid can still update them in place). Modules defined in Android.bp as
# presigned android_app_import -> /product/app. See also docs/backlog.md (default-apps criteria).
PRODUCT_PACKAGES += \
    OrganicMaps \
    Conversations \
    BreezyWeather \
    Fennec \
    Aegis \
    NewPipe \
    AntennaPod \
    Catima \
    Notally

# Native JNI libs for the default apps (DIA-20260618-01). F-Droid ships them compressed with
# extractNativeLibs=true; a verbatim (preprocessed) system app never unpacks them, so dlopen fails at
# launch (Fennec/OrganicMaps/AntennaPod crash; NewPipe's lazy load would too). fetch-default-apps.sh
# extracts each APK's arm64 .so into prebuilts/jni/<App>/arm64 and generates this fragment, which copies
# them into the app's /product lib dir so the loader resolves them -- keeping the APK (and its F-Droid
# signature) byte-identical. Generated + .gitignored: absent on a clean checkout until fetch runs (same
# prerequisite as the .apk files themselves, so a build that has the APKs also has this).
-include vendor/diaspore/prebuilts/default-apps-jni.mk

# AOSP OFFLINE geolocation time-zone provider (DIA-20260616-61; docs/timezone-model.md tier 3). The geotz
# APEX (source in packages/modules/GeoTZ) bundles the provider app com.android.timezone.location.provider +
# the bundled S2 tz database, so a GPS fix resolves to an Olson zone ENTIRELY ON-DEVICE (no GApps, no network)
# -- the privacy-preferred auto source. It is NOT in the FP3 image by default; include it here, then the
# config overlay (vendor/diaspore/overlay/.../config.xml) points config_primaryLocationTimeZoneProviderPackageName
# at the provider so `time_zone_detector is_geo_detection_supported` flips true and Automatic timezone can use it.
# The com.android.geotz APEX ships ONLY the tz data (tzs2.dat) + the system_server classpath jar (geotz.jar) --
# NOT the provider app. So also install the provider app standalone (OfflineLocationTimeZoneProviderService, a
# privileged app -> needs its privapp-permissions allowlist or the build's privapp check fails). This module is
# the SELF-CONTAINED "app" variant (GeoTZ/app): it embeds its own tzs2.dat as an APK resource and copies it to
# its files dir at init, so it does NOT depend on the APEX data path. (DIA-20260616-61.)
PRODUCT_PACKAGES += \
    com.android.geotz \
    OfflineLocationTimeZoneProviderService \
    privapp-permissions-com.android.timezone.location.provider

# Pre-grant the geo provider its location RUNTIME permissions (DIA-20260617-02). privapp-permissions only grants
# signature|privileged perms, NOT dangerous runtime perms -- so without this the provider crashes with
# SecurityException at getCurrentLocation(), AM backs its restart off to hours, and geo never resolves a fix
# (supported/enabled=true but the algorithm stays INITIALIZING/UNCERTAIN forever). A default-permissions
# exception grants ACCESS_FINE/COARSE/BACKGROUND_LOCATION to the package at EVERY user's creation (the gate user
# 0 AND every ephemeral roamed user -- the provider binds per-current-user via ServiceWatcher), which is exactly
# where DIA-20260617-01 turns geo detection on. Proven on FP3: clean boot -> provider healthy -> mock location
# resolves to its Olson zone (Tokyo/London), state CERTAIN. See the XML header + docs/timezone-model.md.
PRODUCT_COPY_FILES += \
    vendor/diaspore/default-permissions/nowhere-geotz-location.xml:$(TARGET_COPY_OUT_SYSTEM)/etc/default-permissions/nowhere-geotz-location.xml

# Pre-grant POST_NOTIFICATIONS to the chooser (DIA-20260619-12) so its user-0 process can raise the
# "not backed up" notification IN the foreground roamed user. POST_NOTIFICATIONS is a runtime perm
# (chooser targetSdk 34); privapp-permissions doesn't cover runtime perms, so without this the cross-user
# notify() is dropped in the ephemeral roamed user. Granted at every user's creation, like the geotz grant.
PRODUCT_COPY_FILES += \
    vendor/diaspore/default-permissions/nowhere-chooser-notifications.xml:$(TARGET_COPY_OUT_SYSTEM)/etc/default-permissions/nowhere-chooser-notifications.xml

# Diaspore boot animation: provided via TARGET_BOOTANIMATION in device/fairphone/FP3/BoardConfig.mk
# (= vendor/diaspore/media/bootanimation.zip). LineageOS's own lineage_bootanimation soong module
# (vendor/lineage/bootanimation, genrule "gen-bootanimation.zip") then copies OUR prebuilt and
# installs it to /product/media — a clean override with a single install rule.
# Do NOT PRODUCT_COPY_FILES to /product/media/bootanimation.zip: it collides with that module
# ("error: overriding commands for target ...bootanimation.zip"). Asset built by make-bootanimation.ps1.
# NOTE: bootanimation.zip MUST be a STORED (uncompressed) zip — bootanimation mmaps each frame
# (createEntryFileMap), which fails on deflated entries → blank boot screen. Re-zip with `zip -0`.

# Diaspore branding: rename "LineageOS" -> "Diaspore" in the SetupWizard (incl. the "Welcome to ..."
# screen) via a static resource overlay that overrides SetupWizard's os_name string.
PRODUCT_PACKAGE_OVERLAYS += vendor/diaspore/overlay

# OS version stamp: bake the Diaspore version into /system so the running OS reports it (and OTAs are
# detectable v1 -> v2). dm-verity-protected like the rest of /system.
PRODUCT_COPY_FILES += \
    vendor/diaspore/etc/nowhere-ota-version:$(TARGET_COPY_OUT_SYSTEM)/etc/nowhere-ota-version

# Per-device default timezone + locale (DIA-20260616-58). A no-GApps / no-SIM device has no NITZ and weak
# geolocation-tz, so the framework's timezone detector falls back to the build default -- which was GMT, so
# the clock showed the right INSTANT in the wrong ZONE for any non-GMT user. Bake a sensible market default
# (`persist.sys.timezone`): a fresh `/data` (no /data/property override) comes up in this zone instead of
# GMT. `ro.nowhere.default.{timezone,locale}` are the chooser's fallback CONSTANTS -- on login it applies
# the profile's ROAMED tz/locale, or these defaults when a profile has none (so each login is deterministic
# and the gate-at-rest never inherits the last user's zone after a fresh boot). en-US is already the
# framework locale default; set it explicitly so the constant and the device agree.
PRODUCT_PRODUCT_PROPERTIES += \
    persist.sys.timezone=America/New_York \
    ro.nowhere.default.timezone=America/New_York \
    ro.nowhere.default.locale=en-US

# Per-edition brand shown on the gate (the chooser reads ro.nowhere.brand; core defaults to "nowhere").
PRODUCT_PRODUCT_PROPERTIES += \
    ro.nowhere.brand=diaspore

# Diaspore shutdown animation: the dispersing-spore motif in REVERSE (dots gather inward, wordmark fades
# to nothing) -- "your phone, nowhere" on power-off. bootanimation (BootAnimation.cpp) plays the first of
# /product, /oem, /system /media/shutdownanimation.zip on shutdown. Unlike bootanimation (installed by the
# lineage_bootanimation genrule), nothing else installs a shutdownanimation, so PRODUCT_COPY_FILES is safe
# (no overriding-commands collision). MUST be a STORED zip (mmap), built by make-shutdownanimation.ps1.
PRODUCT_COPY_FILES += \
    vendor/diaspore/media/shutdownanimation.zip:$(TARGET_COPY_OUT_PRODUCT)/media/shutdownanimation.zip

# Device store config (S3 endpoint + creds) is NOT baked into the read-only system image BY DEFAULT: it is
# provisioned out-of-band to /data/nowhere/nowhere.conf (mutable, FBE-encrypted at rest, rotatable
# without a reflash) and read from there by the boot/login/sync/worker scripts (with a /system fallback
# for older devices). Keeping creds out of /system removes a plaintext-at-rest leak and lets the key
# rotate in place. A clean checkout therefore builds an un-enrolled OS regardless. (An OPT-IN bake of the
# store conf -- for a turnkey device that must self-configure across a /data wipe without a first login --
# is available at the bottom of this file; see the nowhere.conf wildcard block + its SECURITY note.) The full discovery /
# bootstrap path (no creds on the device at all) is the productionization -> docs/enrollment.md.

# OPT-IN TURNKEY DEFAULT (fresh-flash self-provision). Bake a device-default DISCOVERY config into /system
# so a freshly flashed / factory-reset device can bootstrap its data store from name+passphrase ALONE: with
# no /data config the login daemon falls back to this /system copy (nowhere_login.sh), and on login it GETs
# a sealed store-config at the profile's unguessable bootstrapRef and auto-applies it -> the gate comes up
# (device-owner/kiosk already self-provision) AND "name+passphrase -> your phone" works with ZERO manual
# store setup. The "flash and it just works" turnkey path.
#
# CONDITIONAL on the file existing: a clean checkout has no etc/discovery.conf (it is .gitignored; only the
# *.example template is tracked), so the default build stays UN-ENROLLED and creds never enter git. A turnkey
# build drops a real etc/discovery.conf (copy the *.example + fill, or reuse the conf backup) and it bakes.
# SECURITY: prefer a KEYLESS discovery.conf (DIA-44) -- DISCO_ENDPOINT + DISCO_BUCKET only, keys BLANK, with
# the discovery bucket public-read. Then this bakes NO creds into /system: the daemon bootstraps over an
# anonymous GET (discovery holds only sealed, zero-knowledge blobs at unguessable refs). If you instead fill
# DISCO_ACCESS_KEY/SECRET (private bucket / dev), you bake a long-lived key into read-only /system -- in the
# dev setup that's the account-level Filebase key (the "dev caveat"); a production private bucket wants a
# SCOPED, least-privilege key. Scoped WRITE tokens (publish) are the Phase-2 broker (docs/enrollment.md,
# security backlog). NOTE: discovery.conf must be LF (CRLF -> trailing \r breaks the agent).
ifneq ($(wildcard vendor/diaspore/etc/discovery.conf),)
PRODUCT_COPY_FILES += \
    vendor/diaspore/etc/discovery.conf:$(TARGET_COPY_OUT_SYSTEM)/etc/nowhere/discovery.conf
endif

# OPT-IN: also bake the full device STORE config into /system (conditional on the file existing). By default
# this is NOT baked -- the store conf is provisioned to /data/nowhere/nowhere.conf, and after a wipe the
# discovery bootstrap (above) re-materializes it on the first LOGIN. But discovery only helps UNLOCK (an
# existing profile); CREATE (a brand-new profile) needs a store already present, so a freshly-wiped device
# with only discovery can't create until it has been unlocked once. Baking the store conf makes the device
# fully self-sufficient across `fastboot -w`: the login daemon's /system fallback (nowhere_login.sh) finds
# it, so the gate shows a configured store and BOTH Unlock and Create work immediately -- no manual restore.
# SECURITY: this bakes the S3 endpoint + access/secret keys into read-only /system (plaintext-at-rest). In
# the dev / turnkey-demo setup that's the account-level Filebase key (the "dev caveat"); production wants a
# SCOPED, least-privilege key (the Phase-2 token broker -- docs/enrollment.md + security backlog). CONDITIONAL
# + .gitignored (only nowhere.conf.example is tracked), so a clean checkout builds an UN-ENROLLED OS and the
# creds never enter git. NOTE: nowhere.conf must be LF (CRLF -> trailing \r breaks the agent).
ifneq ($(wildcard vendor/diaspore/etc/nowhere.conf),)
PRODUCT_COPY_FILES += \
    vendor/diaspore/etc/nowhere.conf:$(TARGET_COPY_OUT_SYSTEM)/etc/nowhere/nowhere.conf
endif
