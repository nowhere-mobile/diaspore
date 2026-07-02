# Diaspore — Phase 1 Spec: Verified Amnesiac Netboot

Status: draft · 2026-06-08 · prerequisite: Phase 0 (loop proven)

## 1. Objective

One GKI phone boots **LineageOS** from a **verified, content-addressed image served over
WiFi**, with `/data` as **tmpfs** so the device is **factory-fresh every boot**. No roaming
state yet — that is Phase 2. This phase turns the Phase 0 loop logic into a real Android
boot, de-risked on the Cuttlefish emulator before crossing to hardware.

## 2. Exit criteria (Phase 1 is done when…)

A real GKI device:
1. boots to the LineageOS UI with `system`/`vendor` **delivered over WiFi** from the
   content-addressed store (no image on the device's own data partition);
2. is **verified** — the system image has a dm-verity hash tree whose root is signed; flip
   one byte and the boot refuses;
3. runs `/data` as **tmpfs** — create accounts/files, reboot, they are gone (amnesiac);
4. is **untethered** — no USB cable to a host is required to boot.

Measured by a scripted demo: boot → write a marker file → reboot → marker gone; and a
tamper test: corrupt a chunk in the store → boot refuses.

## 3. Scope

| In scope | Deferred to |
|---|---|
| Custom `boot`/`init_boot.img` + custom AVB key; bootloader unlock | — |
| Stage-1 initramfs network-root (NBD or chunk-fetch) | — |
| erofs + dm-verity verified system image | — |
| `/data` = tmpfs (amnesiac) | — |
| WiFi in initramfs (the untether) | — |
| Cuttlefish dev track (M1–M4) | — |
| Roaming `/data`, client-side encryption, replication | **Phase 2** |
| Last-known-good local boot, anti-rollback, OS update channels, lazy restore | **Phase 3** |
| SELinux **enforcing** (Phase 1 boots **permissive**), key UX, multi-device | **Phase 4** |
| Cellular as a boot transport | out |

## 4. Strategy — climb a ladder on Cuttlefish, then cross to hardware

Each milestone adds exactly **one** hard thing, so failures are isolable. M1–M4 run on the
**Cuttlefish** virtual device (no hardware, vendor, WiFi, AVB, or unlock risk); M5–M6 move
to one real phone. This is the standard Android dev de-risking path and mirrors how AOSP CI
itself uses Cuttlefish.

## 5. Milestones

### M1 — Cuttlefish baseline & interposition
- **Goal:** boot stock Cuttlefish, and prove we can inject our own first-stage `/init` that
  runs and then chains to Android's init unchanged.
- **Approach:** build/fetch `aosp_cf_x86_64_phone` images + host package; unpack the
  **generic ramdisk** (in `init_boot.img` on Android 13+, else `boot.img`) with
  `unpack_bootimg`; prepend a wrapper `/init` that logs a sentinel then `exec`s the real
  init; repack with `mkbootimg`; `launch_cvd -init_boot_image=…`.
- **Deliverables:** `phase1/cuttlefish/{fetch-images,unpack,repack,launch}.sh`.
- **Exit:** Cuttlefish boots to UI with our sentinel line in the kernel log.
- **Risks:** ramdisk format/compression (lz4 vs gz) per build; `init_boot` vs `boot` target.

### M2 — Amnesiac `/data` (still local)
- **Goal:** make `/data` ephemeral, isolating the "wipe every boot" risk from netboot.
- **Approach:** keep the normal local `system`/`vendor`; in our `/init` (or a generated
  fstab / `init.rc` override) mount `/data` as **tmpfs** instead of the physical userdata.
  Boot, create state, reboot → factory-fresh.
- **Deliverables:** `phase1/cuttlefish/initramfs/init` (+ `fstab.diaspore`).
- **Exit:** Cuttlefish boots amnesiac — `/data` provably wiped each boot; apps tolerate it.
- **Risks:** setup-wizard / keystore expecting durable `/data`; tmpfs sizing; boot
  **permissive** to dodge SELinux labeling for now.

### M3 — Netboot the OS image (local → network) in Cuttlefish
- **Goal:** deliver `system`/`vendor` over the network from the content-addressed store.
- **Approach:** `/init` fetches the OS image from `store_server.py` over the guest's virtio
  network, attaches it, and presents it to Android init. **Key decision (see §6):**
  whole-`super.img` over **NBD** (recommended first — least friction) vs per-partition
  **erofs + generated fstab**.
- **Deliverables:** `phase1/cuttlefish/publish-os.sh` (build + push), `initramfs/init`
  network-root path; reuses `phase0/store/store_server.py`.
- **Exit:** Cuttlefish boots Android with the OS delivered over the network; `/data` tmpfs.
- **Risks:** super/dynamic-partition handling; NBD-in-initramfs timing; image size vs RAM.

### M4 — erofs + dm-verity (verified)
- **Goal:** make the netbooted OS tamper-evident.
- **Approach:** build the system image as **erofs** with a dm-verity hash tree; sign the
  root hash; `/init` sets up dm-verity and **refuses** on root-hash mismatch. Tie into the
  signed-manifest trust anchor baked into the initramfs (boot-flow.md chain of trust).
- **Deliverables:** `publish-os.sh` adds verity+signing; `initramfs/verify.sh`.
- **Exit:** verified netboot in Cuttlefish; flip a stored byte → boot refuses.
- **Risks:** verity hash-tree layout vs erofs; key handling in the dev harness.

### M5 — Real GKI device, tethered (USB-eth + NBD)
- **Goal:** the same boot on real hardware, but transport still tethered (no WiFi yet).
- **Approach:** pick a device; `fastboot flashing unlock`; generate a **custom AVB key**;
  build & flash custom `boot`/`init_boot.img`; port the M1–M4 initramfs; transport via
  **USB-Ethernet + NBD** (the postmarketOS-proven pre-init path).
- **Deliverables:** `phase1/device/{device-prep,flash,netboot-usb-nbd}.md` + scripts.
- **Exit:** real phone boots Android amnesiac + verified, from the network, tethered by USB.
- **Risks:** per-device kernel/DTB/`vendor_dlkm`; AVB "yellow" state UX; unlock voids
  warranty / triggers anti-rollback fuses on some devices.

### M6 — Untether: WiFi in initramfs (Phase 1 exit)
- **Goal:** fetch over WiFi, no host tether.
- **Approach:** bundle the device's WiFi **kernel modules + firmware** into the initramfs;
  bring up `wlan0` + `wpa_supplicant` + DHCP in `/init`; fetch over WiFi; connectivity gate
  with backoff (boot-flow.md §3).
- **Deliverables:** `phase1/device/wifi-initramfs.md`; firmware/module bundling in the
  ramdisk build.
- **Exit = Phase 1 done:** untethered, verified, amnesiac netboot on real hardware.
- **Risks:** WiFi firmware/regulatory blobs; chicken-and-egg (modules needed before network
  → must ship in initramfs); driver quirks; enterprise/EAP networks.

## 6. The crux — delivering `system` and the Android init handoff

The single biggest integration question, surfaced at M2–M3.

**Insight that simplifies it:** in Android, `/system` and `/vendor` are **already
read-only** (mounted from `super` via dm-linear, dm-verity-protected). So "amnesiac" does
**not** require an overlayfs-with-tmpfs-upper over system the way the Phase 0 general-Linux
model framed it. Amnesiac Android ≈ **read-only OS from the network + `/data` = tmpfs**.
That removes a whole class of overlay/SELinux friction from Phase 1.

**Two ways to deliver the OS and hand off to Android init:**

- **(A) Whole-`super.img` over NBD (recommended first).** `/init` attaches `super` as an
  NBD block device fetched from the store; Android's normal first-stage init does its usual
  dynamic-partition + dm-verity mount. Minimal init surgery; `super` is large but
  content-addressed/chunked so deltas are small. Best for getting M3 booting fast.
- **(B) Per-partition erofs + generated fstab.** `/init` sets up each logical partition
  (system/vendor/…) as its own erofs+verity dm device and feeds Android a **generated
  fstab** pointing at them, bypassing `super`. More faithful to the content-addressed design
  and to per-partition delta updates, but more init work.

Recommendation: **A → B**. Boot with (A), then migrate to (B) once stable.

**Handoff mechanism** (independent of A/B): keep Android's first-stage init and override
only *what it mounts* via the generated fstab / device paths — inherit Android's SELinux +
second-stage handoff for free, rather than the `INIT_SECOND_STAGE` skip-ahead path.

## 7. Hardest rung — WiFi in initramfs (M6)

Why it's last and hardest: the WiFi driver + firmware live in the vendor partitions, but
we need the network *before* we can fetch anything — so they must be **bundled into the
initramfs** (chicken-and-egg). Plan: extract the device's `vendor_dlkm` WiFi module(s) +
`/vendor/firmware` blobs at image-build time, stage them in the ramdisk, `modprobe` in
`/init` before `wpa_supplicant`. Fallback for bring-up: USB-eth (M5) stays available.

## 8. Risk register (top items)

| Risk | Mitigation |
|---|---|
| Android init handoff harder than expected (M2/M3) | Split into M2 (local, `/data` tmpfs) before M3 (network); start with super.img/NBD (option A) |
| No Linux/KVM host for Cuttlefish | Cloud nested-virt VM (AOSP-CI-style) or local bare-metal Linux; **decision §9** |
| Device-specific WiFi blobs (M6) | Keep USB-eth (M5) as fallback; pick a well-documented device |
| SELinux denials | Boot **permissive** all of Phase 1; tighten in Phase 4 |
| Bootloader unlock side-effects (M5) | Pick a device with clean unlock (Pixel); accept Play Integrity loss (already accepted) |
| Image size vs RAM in initramfs | Lazy/NBD block delivery, not full download-to-RAM |

## 9. Decisions needed before M1

1. **Cuttlefish host** — M1–M4 need a Linux x86 host with KVM. Cloud nested-virt VM /
   local bare-metal Linux / WSL2 (finicky)?
2. **Target device** — sets kernel/DTB/WiFi-firmware for M5–M6. Pixel 6/7/8 is the clean
   default (easy unlock, GKI, strong LineageOS + tooling support).
3. **Transport** — NBD (block) first vs casync chunk-fetch (file). Recommend **NBD** first.
4. **Super strategy** — whole `super.img` (option A) first vs per-partition erofs (B).
   Recommend **A → B**.

## 10. Proposed repo layout (created as M1 starts)

```
phase1/
├── README.md
├── cuttlefish/
│   ├── 00-setup.md             host prereqs, build/launch Cuttlefish
│   ├── fetch-images.sh  unpack.sh  repack.sh  launch.sh
│   ├── publish-os.sh           build erofs(+verity) image, push to store
│   └── initramfs/
│       ├── init                stage-1 /init (evolves from docs/boot-flow.md)
│       ├── fstab.diaspore      generated/static fstab for the handoff
│       └── verify.sh           dm-verity setup + signature check
├── device/
│   ├── 00-device-prep.md       unlock, custom AVB key, flashing
│   ├── netboot-usb-nbd.md      M5 tethered transport
│   └── wifi-initramfs.md       M6 untether (modules/firmware/wpa)
└── docs/
    └── android-init-handoff.md  detailed notes on §6
```

## 11. Verification per milestone

Each milestone ships a one-command check, mirroring Phase 0's harness discipline:
M1 sentinel-in-log; M2 marker-gone-after-reboot; M3 boots with store stopped→fails,
started→succeeds; M4 corrupt-chunk→refuses; M5/M6 the full scripted demo (write→reboot→gone)
plus tamper test, on device.
