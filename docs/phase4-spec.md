# Diaspore — Phase 4 Spec: Real Hardware

Status: draft · 2026-06-09 · prerequisite: **Phase 3 COMPLETE** (whole thesis validated on Cuttlefish)

Phase 4 leaves the emulator. Everything is proven in principle; Phase 4 is **productionization on a
real phone** — no new core unknowns, but real build + real flashing.

## The split (what's drivable here vs what needs your device)
- **Build** — produce the real Diaspore OS image. Needs a build host, **not** a phone. I can drive this.
- **Flash + validate** — unlock → `fastboot flash` → boot → validate → lock. Needs **your physical
  phone**; the flashing is physical/irreversible-ish, so I provide exact checked steps and you run them.

## Device selection
Requirements: GKI/Treble, **unlockable bootloader**, A/B, strong LineageOS support, and — for locked
verified boot under *our* key — `avb_custom_key` support.
**DEVICE DECIDED (2026-06-09): Fairphone 3 (codename `FP3`).** On-brand (repairable, pro-openness) and
verified viable:
- ✅ **LineageOS** — officially supported + maintained (device tree `WeAreFairphone/android_device_fairphone_FP3`).
- ✅ **Bootloader unlock** — Fairphone issues official unlock codes.
- ✅ **A/B** — yes (`boot_a`/`boot_b`, seamless; recovery-in-boot). So P3.3's A/B OTA model applies.
- ✅ **Custom AVB key + re-lock** — yes → **YELLOW** verified boot (locked + verified under the Diaspore
  key, with a custom-key warning screen). Yellow is the *correct* AVB state for a user key (green is
  OEM-only by design), so this **crosses the P3.2 boundary** Cuttlefish couldn't. Bonus: stock FP3's
  root of trust is the **Google AVB test key**, so our test-key path also works for bring-up.
- ⚠️ **Non-GKI** (2019, Snapdragon 632, kernel ~4.9) — there is **no `init_boot`**; the stage-1 ramdisk
  lives in **`boot.img`**. Our M2 init-interposition applies, just targeting `boot.img`'s ramdisk.

**FP3 deltas vs the Pixel-centric ladder:** P4.2 init interposition targets `boot.img` (not `init_boot`);
P4.3 yields **yellow** (not green) locked verified boot under our key — expected for a custom key, not a
limitation. Everything else (A/B OTA, agent bake, chooser, roaming, amnesiac) maps directly.

FP3 is the **flagship pilot** (full Diaspore + locked verified boot). For how this scales to *most*
devices without one-image-per-phone, see **[portability.md](portability.md)** (GSI for reach + a short
flagship list + LineageOS-ride for the long tail).

## Milestone ladder
- **P4.1 — Build host.** Provision (~16+ vCPU, 64 GB RAM, 400 GB+ disk). Sync LineageOS for the device.
- **P4.2 — Bake Diaspore in (faithful).** The real version of P3.0's deferred bake, done in the build:
  agent in `/system` + `diaspore.rc` init service **under SELinux enforcing** (proper sepolicy), the
  chooser as a system app (P3.1), and the stage‑1 init interposition (M2) as a proper init customization.
  This is where the erofs/super surgery lives — it's a build step now, not hand-patching.
- **P4.3 — Custom AVB key, end to end. ✅ DONE on the FP3 (DIA-20260613-07).** Re-enabled dm-verity,
  re-signed the chain with the custom Diaspore key, `fastboot flash avb_custom_key` + `flashing lock`
  → **yellow** locked verified boot under our key (`verifiedbootstate=yellow`, `flash.locked=1`,
  `veritymode=enforcing`), roam cycle works locked. The realized procedure (incl. the
  flash-both-slots + post-lock factory-reset gotchas) is the runbook at
  [`../phase4/build/p4.3-verified-boot.sh`](../phase4/build/p4.3-verified-boot.sh). Note: FP3 yields
  **yellow** (custom key), not green (green needs Fairphone's OEM key). Follow-up: securely back up the
  custom AVB private key — it is now the device's root of trust.
- **P4.4 — Real OTA. ✅ DONE on the FP3 (DIA-20260613-08).** `m otapackage` (testkey-signed full A/B
  payload) → applied via `update_engine` on the **locked** device with no unlock → AVB-verified under
  our custom key (stays yellow) → slot switch → **reboot-into-v2 + A/B rollback** both ways, `/data`
  untouched. Runbook: [`../phase4/build/p4.4-ota.sh`](../phase4/build/p4.4-ota.sh). The
  content-addressed delta transport (P3.3: payload as CDC chunks) is the follow-on **P4.4b**.
- **P4.5 — Flash + validate the loop.** Unlock → flash → boot → blind-login chooser → roam restore over
  real WiFi → use → power-off wipe. Validate amnesiac + roaming + verified boot on a real phone.
- **Exit:** a real Pixel running Diaspore — verified local OS under our key, blind-login chooser,
  roaming encrypted user state, amnesiac on power-off, content-addressed OTA.

## What you'll need to provide / decide
1. **The device** — a bootloader-unlockable phone (a Pixel, recommended).
2. **The build host** — bigger than the Cuttlefish VM (LineageOS build is large).
3. **Acceptance:** unlocking wipes the device; some banking/DRM breaks (the known Play-Integrity tradeoff).

## Risks / notes
- LineageOS build is heavy (hours, hundreds of GB).
- Locked verified boot under a custom key is **Pixel-clean** (`avb_custom_key`); other devices vary widely.
- Physical flashing can brick on a wrong image — steps will be exact + verified, and A/B gives a fallback.
- The Cuttlefish VM is **not** the Phase 4 build host (too small) and is otherwise idle now → pause it.

## Immediate next step
Decide the **device** + **build-host approach**, then **P4.1**: stand up the build host and start the
LineageOS sync for the chosen device.
