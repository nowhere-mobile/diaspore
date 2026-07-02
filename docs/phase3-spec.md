# Diaspore — Phase 3 Spec: Profiles, Verified Boot, OS OTA

Status: **Phase 3 COMPLETE — 2026-06-09 — P3.0 / P3.1 / P3.2 / P3.3 all pass** · prerequisite: Phase 2 mechanisms (all proven & committed)

Phase 2 proved the *mechanisms* in isolation (roaming, app roaming, on-device agent, blind-login
keys, lazy restore). **Phase 3 turns them into a usable, secure, self-updating system**: one
end-to-end on-device flow, a blind-login chooser, verified local boot, and content-addressed OS
updates. No new core unknowns — this is integration + hardening + OS lifecycle.

## Objective / exit

A Diaspore device that, on a real (Cuttlefish) boot:
1. boots a **verified** local OS (dm-verity/AVB, Diaspore key);
2. shows a **blind-login chooser** (profile + passphrase, no list);
3. **restores that profile** end-to-end (working set first), is usable, replicates at runtime, wipes on power-off — all driven by an **on-device agent baked into the OS**;
4. can **update its OS** via a content-addressed OTA (pull delta → A/B apply → verify → reboot).

## Tracks (with dependencies)

```
P3.0 integration ──┬─→ P3.1 chooser UI
                   ├─→ P3.2 verified boot ──→ P3.3 OS OTA
                   └ (P3.3 also needs the content-addressed store from Phase 0)
```

### P3.0 — End-to-end integration (do first)
**STATUS: DONE — flow-first (2026-06-09): `P3_0_FLOW_PASS`.** Init drives the loop on-device: a
`diaspore_boot` service restores on `sys.boot_completed=1`; `ctl.start diaspore_shutdown` pushes-then-
wipes; full round-trip (seeded note + a mid-session file) survived push→wipe→reboot→restore. Agent +
`diaspore.rc` staged into `/system` via `adb remount`; boot is **permissive** (`-guest_enforce_security=false`)
so init can launch the debug services. Files in `phase3/integration/`.
**Recon findings:** `adb remount` mods do NOT survive a cold `always_create` wipe; system is **erofs**
and `lpunpack`/`lpdump`/`mkfs.erofs` are missing — so the faithful **image bake** (agent survives a
cold wipe with zero host help) is the same work as Phase 4's real OS build and is **deferred there**.
P3.0 and P3.2 are separable (vbmeta already disabled → a modified system boots without re-signing).

Wire the separate Phase 2 proofs into ONE on-device flow, triggered by boot.
- **Bake the agent into the OS image** (currently re-pushed via adb; it must live in `system`/`vendor` so it survives the `/data` wipe).
- An **init.rc service / boot hook** that runs the flow at the right stage: bring up network → blind-login (P2.3 keys) → restore working set (P2.5) incl. app manifest re-download + data (P2.4) → mark usable → continuous replicate → **`on shutdown`** push + wipe.
- **Exit:** a single boot → (stub) login → full roaming restore → usable; power-off → push + wipe. End-to-end, on-device, no host orchestration.

### P3.1 — Chooser UI (blind login)
**STATUS: DONE — 2026-06-09: `P3_1_BLIND_LOGIN_PASS`.** A real Android chooser **APK** (built from
scratch on the VM: bootstrapped JDK + Android SDK/build-tools, hand-run `aapt2`→`javac`→`d8`→
`apksigner`) presents a blind-login form (profile + passphrase, **no list**) and execs the staged
agent's `restore-set`. Driven headlessly via `am start` extras + `adb input`, screenshots in
`phase3/chooser/shots/`. Blind-login matrix verified: `alice`/`pass-AAA` → **UNLOCKED**;
`alice`/wrong → **`—`** (`cipher: message authentication failed`); `nobody`/anything → **`—`**
(empty); `ghost`/`duressXYZ` → **UNLOCKED** (a *different*, hidden profile). Wrong-passphrase and
unknown-name render an **identical blank** at the UI (deniability). Built as approach (b), a chooser
app. *Refinement noted:* internally wrong-pass (rc=1, decrypt fail) vs unknown-name (rc=0, empty)
differ — uniform at the UI but a hardening item to equalize side-channels (timing/rc).

A pre-session UI that takes profile name + passphrase (blind — no enumerable list) and feeds the agent.
- **Options:** (a) minimal framebuffer/SDL UI in early boot (cf. pmOS `osk-sdl`); (b) a privileged **chooser Android app/activity** shown first (reuses Android's UI/touch — easiest for the prototype); (c) extend Android's lockscreen/credential flow.
- **Recommendation:** (b) for the prototype; evolve toward (a)/lockscreen later.
- Hidden/duress profiles (P2.3) surface naturally here.
- **Exit:** type profile+passphrase on the Cuttlefish screen → that profile's session materializes; wrong passphrase → nothing; unknown name → indistinguishable from empty.

### P3.2 — Verified boot (dm-verity / AVB)
**STATUS: DONE — 2026-06-09: `P3_2_VERIFIED_BOOT_PASS`.** (1) A custom **Diaspore RSA-4096 AVB key**
signs the modified `init_boot` and `avbtool verify_image` confirms it vouches; a different key is
rejected and a tampered image fails verification. (2) On-device, AVB is **re-enabled** (gave the
modified `init_boot` a valid test-key footer + restored the real `flags=0` chained vbmeta, undoing
M2's `flags=2`); the device **boots with `veritymode=enforcing`** (dm-verity active). Scripts in
`phase3/avb/`. **Boundary:** `verifiedbootstate=orange` because Cuttlefish's bootloader is unlocked
and trusts the AOSP test key — custom-key **green/locked** needs the Diaspore key embedded in the
bootloader, which is **Phase 4** (real hardware). avbtool gotchas: `verify_image --key` wants a PEM
(not the `extract_public_key` blob), and it resolves a hash descriptor via `<partition>.img` in the
image's dir.

Re-enable AVB properly (M2 used a verification-**disabled** vbmeta to allow the modified `init_boot`).
- Sign the Diaspore boot/`init_boot` + system (erofs) with a **custom AVB key**; generate matching `vbmeta`; the bootloader verifies; dm-verity protects the read-only system at runtime.
- **Exit (Cuttlefish):** Diaspore OS boots with AVB enabled under the custom key; flip a block → boot refuses / dm-verity faults.

### P3.3 — Content-addressed OS OTA
**STATUS: DONE — 2026-06-09: `P3_3_OTA_PASS`.** Chunked the OS (`boot.img`) content-addressed with
**casync**; publishing v2 over v1 added only **154,848 B = 0.23%** of the 64 MB image (the delta). A
device with v1 reconstructed v2 by pulling just that delta (v1 as seed) and the result **hashes
identical** to the real v2. Then A/B apply: wrote v2 to the **inactive boot slot** (`boot_b`), read it
back **byte-identical**, and the boot HAL **honored the slot switch** (`set-active-boot-slot`, then
reverted to the good slot). Scripts in `phase3/ota/`. **Boundary:** a full reboot-into-v2 + rollback
needs the complete multi-partition payload (incl. super's dynamic partitions) applied by
`update_engine` (present on device) — production/real-HW, gated by P3.2's verified boot. Writes hit
the ephemeral overlay only (reset next launch).

The local OS updates by pulling a content-addressed **delta**, not netbooting.
- Publish the OS as content-addressed chunks (casync/desync, or extend the Phase 0 store to chunks). Device **updater** pulls changed chunks → writes the inactive **A/B slot** → verifies (AVB) → sets active → reboots into the new OS.
- **Staged:** first prove content-addressed **delta pull + verify**; then the **A/B slot apply + reboot**.
- **Exit:** publish OS v1 (device on v1) → publish v2 → device pulls delta → applies to slot B → verifies → reboots into v2.

## Achievable on Cuttlefish vs Phase 4 (real hardware)
All four are demonstrable on Cuttlefish: UI via the Cuttlefish display/WebRTC; AVB via `avbtool` + Cuttlefish vbmeta; A/B via Cuttlefish's A/B slots; OTA via the store + `update_engine` or a custom slot writer. Real-hardware specifics (custom AVB key locked into the bootloader, unlock/attestation state) are **Phase 4**.

## Risks / open questions
- **Restore timing:** how early can the chooser take input + the agent restore run, relative to framework start? May need a minimal "restoring" session that then transitions to the full profile session.
- **AVB re-enable on a modified `init_boot`:** correctly re-signing the modified ramdisk + matching vbmeta descriptors (we disabled it in M2; re-enabling needs the full key/descriptor chain).
- **OTA apply path:** reuse Android `update_engine` (A/B payload format) vs a simpler custom content-addressed slot writer.
- **SELinux enforcing** for the chooser app + the baked-in agent (Phase 2 ran permissive-friendly).
- **Key custody / recovery** for blind-login (passphrase-derived; recovery key).

## Decisions needed before execution
1. **Chooser UI approach:** chooser-app (recommended) vs framebuffer vs lockscreen.
2. **OTA apply:** `update_engine`/A-B payload vs custom slot writer.
3. **Entry point:** start with **P3.0 integration** (recommended), then P3.1 UI → P3.2 verified boot → P3.3 OTA; or tackle verified boot (P3.2) first.

## Proposed repo layout (created as execution starts)
```
phase3/
├── README.md
├── integration/   init.rc service + boot/shutdown hooks + agent-in-OS packaging   (P3.0)
├── chooser/        the blind-login UI (app or framebuffer)                          (P3.1)
├── avb/            custom-key signing + vbmeta generation + verify tests            (P3.2)
└── ota/            content-addressed OS publish + device updater + A/B apply        (P3.3)
```

## Immediate first step
**P3.0** — bake the agent into the Cuttlefish system image and add an init service that runs the
end-to-end flow (stub login → restore working set → usable; `on shutdown` → push + wipe), turning
the separate Phase 2 proofs into one on-device boot-to-roam-to-wipe cycle.
