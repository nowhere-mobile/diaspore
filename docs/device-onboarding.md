# Device onboarding — adding a phone to Diaspore or Endospore

Status: **template / checklist (2026-06-30).** Cross-edition: applies to both **Diaspore** (LineageOS) and
**Endospore** (GrapheneOS). Pairs with restructure.md (the core/editions/devices topology),
[endospore-spec.md](endospore-spec.md), and [roadmap.md](roadmap.md).

## The model: a device is cheap, a fork is not
Per the `core/` + `editions/<edition>/` + `devices/<codename>/` topology, **adding a device adds ONLY**
`editions/<edition>/devices/<codename>/` — its delta (home-grid overlay, per-device source patches, per-device
AVB keys) — plus a build target. `core/` and the edition root are untouched, and a second device for an OS is
**never a fork**. So the cost per device is bounded; the *risk* is entirely in the one gate below.

## The one gate that sets the assurance tier: custom-key RELOCK
Everything in the security pitch *except the secure element* comes from **verified boot under OUR AVB key** —
which needs relocking the bootloader against a *custom* key. **This is device-dependent, and it's the first
thing to confirm**, because it splits devices into two tiers:

- **Full assurance** — relocks against our custom AVB key → genuine verified boot + rollback protection
  (boot shows *yellow* with our fingerprint). **Pixels** (the GrapheneOS path) and **Fairphones** support
  this. This is the real product.
- **Reduced assurance** — unlocks but **won't relock against a non-OEM key** (most Motorola / Samsung /
  OnePlus). You still get **amnesiac roaming** + the managed store, but **not** verified boot under our key.
  Honest marketing must label these models accordingly.

**Test relock EARLY**, before investing in the overlay/build — it's make-or-break.

## Per-device checklist
1. **Upstream OS support.** On the LineageOS device list (Diaspore) or GrapheneOS supported devices
   (Endospore)? No official support → out of scope (don't bring up an unmaintained tree).
2. **Bootloader unlock.** Confirm `fastboot flashing unlock` works (OEM unlocking allowed).
3. **🚩 Custom-key RELOCK.** Confirm `fastboot flash avb_custom_key` + `fastboot flashing lock` → boots
   *yellow* under our key (full tier) vs refuses/wipes (reduced tier). **Records the tier.**
4. **Vendor blobs.** Extract proprietary firmware (Pixel: `adevtool`; LineageOS: the device tree's
   `extract-files`); confirm the redistribution stance.
5. **Per-device keys.** Generate this device's releasekey / platform / AVB / OTA keys — **off-repo**, backed
   up at `C:\Linnae\keys\<edition>-<codename>` (never reuse another device's keys).
6. **Device overlay.** Add `editions/<edition>/devices/<codename>/`: the launcher home-grid layout
   (`default_workspace_NxM.xml`) sized to the screen, an `os-ref`, and any per-device source patches (icon
   size / grid). `stage-vendor.sh <repo> <dest> <codename>` overlays it on the OS-common tree.
7. **Build target.** Wire the codename into the edition `.mk` + the build (`breakfast <codename>` for
   LineageOS; for Endospore, `inject-product.sh` into the adevtool-generated device `.mk`).
8. **Build + flash + VALIDATE THE LOOP on hardware** — the non-negotiable: gate → blind login → roaming
   restore → power-off wipe → re-login restore → OTA self-update → (full tier) relock + re-validate. Use the
   reference device's validation table.
9. **Record the tier** (full / reduced verified boot; SE present?) in the device README + the storefront
   comparison, so the per-model assurance claim stays accurate.

## Current + planned device matrix
| Device | Codename | Edition | OS | Relock / tier | Status |
|---|---|---|---|---|---|
| Fairphone 3 | `fp3` | Diaspore | LineageOS | yellow / **full** (no SE) | ✅ shipped — the reference |
| Pixel 7a | `lynx` | Endospore | GrapheneOS | yellow / **full** + Titan M2 | ✅ E.0–E.4 proven; catch-up = E.5 |
| Pixel 10a | TBD | Endospore | GrapheneOS | **full** + Titan | planned #1 — strongest target |
| Fairphone 6 | TBD | Diaspore | LineageOS | yellow / **full** | planned — natural FP3 successor |
| Moto G (2025) | TBD | Diaspore | LineageOS | **⚠️ relock TBD** | planned — **confirm relock FIRST**; likely reduced tier |

Add a row + a `devices/<codename>/` dir per new device; nothing else in the tree moves. Prioritize by the
**assurance you can actually deliver** (full-tier devices first), and be explicit when a model is reduced-tier.
