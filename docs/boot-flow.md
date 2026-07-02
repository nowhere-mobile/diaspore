# Diaspore — Stage-1 initramfs network-root flow

Status: draft · 2026-06-08

This is the custom `/init` (PID 1) inside the stage-1 ramdisk. It sits between the kernel
and Android's own init: it does the networking the bootloader can't, assembles the root,
then hands off.

> **Refined model (2026-06-09):** the OS is now **stored locally**, not netbooted — so the
> initramfs no longer needs WiFi or to fetch the OS. The stage-1 init's remaining job shrinks to
> setting up an **ephemeral `/data`** + the blind-login **chooser** (keys); the **roaming
> user-state restore runs later, in Android userspace** (network up, since `/data` is `latemount`).
> The network-root flow below is retained as the **optional "untethered netboot"** stretch
> (roadmap Phase 5). See **[roadmap.md](roadmap.md)** and **[app-model.md](app-model.md)**.

## Where it sits

```
bootloader -> boot.img (kernel + THIS initramfs)
                kernel unpacks initramfs, execs /init  <- you are here
                /init: pseudo-fs -> modules -> network -> fetch+verify image
                       -> build overlay root at /newroot -> switch_root -> Android init
```

## Kernel cmdline (baked into boot.img or set by bootloader)

```
androidboot.slot_suffix=_a androidboot.hardware=<soc> androidboot.serialno=...
nr.channel=stable                       # which OS ref to resolve
nr.cache=https://cache.example/store    # distributed cache / store endpoint
nr.manifest=https://cache.example/refs  # signed ref->hash manifest endpoint
nr.net=wlan                             # wlan | usbeth | static
nr.mode=roaming                         # amnesiac | roaming
nr.fallback=local                       # what to do if network dies
```

The `nr.*` are custom params; the `androidboot.*` ones must survive to Android init.

## The /init sketch (annotated)

```sh
#!/bin/sh
# PID 1 inside the stage-1 ramdisk. Toybox/busybox userland.
set -eu
log() { echo "[nr] $*" > /dev/kmsg; }
panic() { log "FATAL: $*"; exec /bin/sh; }   # recovery shell, don't reboot-loop

# --- Stage 1: minimal environment ---------------------------------------------
mount -t proc     proc     /proc
mount -t sysfs    sysfs    /sys
mount -t devtmpfs devtmpfs /dev
mount -t tmpfs    tmpfs    /run
mkdir -p /run /newroot /ro_os /overlay
read_cmdline() { sed -n "s/.*\b$1=\([^ ]*\).*/\1/p" /proc/cmdline; }
NET=$(read_cmdline nr.net); CACHE=$(read_cmdline nr.cache)
CHAN=$(read_cmdline nr.channel); MODE=$(read_cmdline nr.mode)

# --- Stage 2: drivers/firmware needed to reach the network --------------------
# WiFi modules + firmware are bundled IN this initramfs (the whole point: the
# network stack exists here, not in the bootloader).
for m in cfg80211 mac80211 <soc_wifi_module>; do modprobe "$m" || panic "modprobe $m"; done
for i in $(seq 1 50); do [ -e /sys/class/net/wlan0 ] && break; sleep 0.1; done

# --- Stage 3: networking (the part a bootloader cannot do) --------------------
case "$NET" in
  wlan)
    ip link set wlan0 up
    wpa_supplicant -B -i wlan0 -c /etc/wpa.conf || panic "wpa assoc"
    udhcpc -i wlan0 -t 5 -n        || panic "dhcp"
    IFACE=wlan0 ;;
  usbeth)                            # fallback: USB-C ethernet / RNDIS gadget
    ip link set usb0 up; udhcpc -i usb0 -t 5 -n; IFACE=usb0 ;;
  static)
    ip addr add "$(read_cmdline nr.ip)" dev wlan0; ip link set wlan0 up; IFACE=wlan0 ;;
esac
ok=0; for i in 1 2 4 8 16; do curl -fsS "$CACHE/health" && { ok=1; break; }; sleep "$i"; done
[ "$ok" = 1 ] || boot_fallback   # see resilience section

# --- Stage 4: resolve channel -> signed image descriptor ----------------------
# manifest is tiny; verify its signature with a trust anchor BAKED INTO this
# initramfs (the initramfs itself is measured/signed by verified boot on /boot).
curl -fsS "$(read_cmdline nr.manifest)/$CHAN"     -o /run/manifest.json
curl -fsS "$(read_cmdline nr.manifest)/$CHAN.sig" -o /run/manifest.sig
openssl dgst -sha256 -verify /etc/trust/anchor.pub \
        -signature /run/manifest.sig /run/manifest.json || panic "manifest sig"
IMG_INDEX=$(jq -r .image_caidx   /run/manifest.json)
VERITY_ROOT=$(jq -r .verity_root /run/manifest.json)

# --- Stage 5: pull + verify the immutable OS image from the cache -------------
# Lazy, content-addressed: only missing chunks fetched, cached locally on a small
# flash partition so the next boot is fast.
mkdir -p /mnt/oscache
mount /dev/disk/by-partlabel/oscache /mnt/oscache 2>/dev/null || true  # safe: public/immutable
desync mount-index --store "$CACHE" --cache /mnt/oscache "$IMG_INDEX" /run/os.img
veritysetup open /run/os.img verity-os /run/os.hashtree "$VERITY_ROOT" \
  || panic "verity root mismatch"    # tamper-evident even if the cache is untrusted
mount -o ro -t erofs /dev/mapper/verity-os /ro_os

# --- Stage 6: assemble the writable overlay root ------------------------------
mount -t tmpfs tmpfs /overlay        # RAM-backed upper = auto-wiped on power-off
mkdir -p /overlay/upper /overlay/work
mount -t overlay overlay \
  -o lowerdir=/ro_os,upperdir=/overlay/upper,workdir=/overlay/work /newroot

if [ "$MODE" = roaming ]; then
  prompt_or_read_key > /run/userkey         # key derived from a secret NOT destroyed by wipe
  desync extract --store "$CACHE/userstate" "$(jq -r .userstate_caidx /run/manifest.json)" /run/data.enc
  cryptsetup open --key-file /run/userkey /run/data.enc data && mount /dev/mapper/data /newroot/data
  echo "$(jq -r .userstate_version /run/manifest.json)" > /run/restored.version
else
  mount -t tmpfs tmpfs /newroot/data        # truly ephemeral
fi

# --- Stage 7: hand off to Android init ----------------------------------------
mount --move /proc /newroot/proc; mount --move /sys /newroot/sys; mount --move /dev /newroot/dev
exec switch_root /newroot /system/bin/init       # <- the fiddly seam, see below
```

## The Android-init handoff (the genuinely hard seam)

Everything above is generic Linux. The friction: Android's *own* first-stage init normally
does the partition discovery we just replaced (mount `super`'s logical partitions, set up
AVB/dm-verity, then `switch_root` to second stage). Two ways to interpose:

- **(a) Skip to second stage.** `switch_root` into init with the environment that says
  first-stage is done — Android init re-execs itself with `INIT_SECOND_STAGE=true`. You
  must present what second-stage expects on `/newroot`: SELinux policy files (in the image),
  populated `/dev`, mounted `/proc` and `/sys`.
- **(b) Reuse Android's first-stage, swap *what* it mounts.** Keep Android's first-stage
  init but feed it a **generated fstab** whose `system`/`vendor`/`userdata` entries point at
  *your* dm devices (`/dev/mapper/verity-os`, `/dev/mapper/data`). You inherit Android's
  SELinux + second-stage handoff for free and only override device resolution. Usually the
  better path on real devices.

Two sub-issues to budget for:
- **SELinux.** Android boots enforcing. overlayfs + SELinux needs correct labeling
  (`context=`/`defcontext=` mount options, or policy that allows the labels). For bring-up,
  boot **permissive**, get it stable, then tighten.
- **Vendor modules/firmware.** On GKI the WiFi driver + firmware live in
  `vendor_dlkm`/`vendor_boot`, but you need them *before* you have the network — chicken-and-egg.
  So they must be **bundled into this initramfs** (or a tiny local vendor partition).

## Profiles & the chooser stage (Phase 2)

Multiple roaming identities can share the device. A **chooser** runs after Stage 3
(network up) but before the Stage 6 `/data` restore — so the profile is chosen *before any
user data is downloaded*. The chooser is a shared, immutable, amnesiac image (no user data),
fitting the immutable-OS tenet.

- **Blind login:** no list is shown; the user types `profile + passphrase`. The typed name
  selects the `head:<identity>` ref and the passphrase derives that profile's keys — feeding
  exactly the `prompt_or_read_key` / identity inputs already used in Stage 6. No enumeration;
  works on a blank device; enables hidden / duress profiles.
- **Flow insert:**
  ```
  Stage 3 (network up)
    -> CHOOSER: read profile name + passphrase (blind)   # no /data yet
    -> derive keys, set IDENTITY=<typed name>
  Stage 5/6: fetch SHARED OS (cacheable, profile-independent)
             restore ONLY head:<IDENTITY> into /data
  ```
- **Switching** = reboot -> choose another profile; the amnesiac wipe guarantees no residue.
- **UI cost:** a minimal framebuffer/touch picker in the initramfs (cf. pmOS `osk-sdl`), or a
  stripped chooser-Android that kexecs into the selection. This is the only new build work.

Phase 1 ships single-profile amnesiac and just reserves this hook.

## Resilience / fallbacks (the `boot_fallback` above)

- **Last-known-good local boot.** If manifest fetch or chunk pull fails, boot the OS image
  already in the `oscache` partition (still verity-checked against the last known-good root
  hash). Network gives updates; it must not be a hard dependency to boot.
- **Timeouts + backoff** on every network step; never an unbounded wait.
- **Recovery shell**, not a reboot loop, on fatal errors (`exec /bin/sh`). Optionally
  `reboot bootloader` after N failures.
- **Verify-before-trust everywhere:** signed manifest (baked-in anchor) → content-addressed
  chunks (hash = name) → dm-verity at runtime. The cache can be fully untrusted; only the
  trust anchor in the signed initramfs must be protected (verified boot on `/boot` covers that).

## What ships inside the initramfs

- toybox/busybox, `ip`, `wpa_supplicant`, `udhcpc`, `curl`, `jq`, `openssl`
- `desync`/`casync` (or `nbd-client`), `cryptsetup`/`veritysetup`, `dmsetup`
- WiFi/USB-eth **kernel modules + firmware blobs**
- the **trust anchor pubkey** and `wpa.conf` (SSID/PSK or EAP creds — or prompt at runtime)

## The shutdown half (registered, runs later)

The flow above only handles *pull*. The assembled root carries an init.rc service that does
**continuous encrypted delta replication** of `/newroot/data` to `$CACHE/userstate` during
runtime, plus an `on shutdown` hook that does a final freeze → snapshot → flush →
verify-retrievable → wipe. Continuous replication is what keeps the power-loss data-loss
window small; relying on a shutdown-only push is the fragile version.
