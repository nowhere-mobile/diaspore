# Diaspore — Portability & Device Support (how many images?)

Status: design · 2026-06-10 · pairs with [phase4-spec.md](phase4-spec.md)

> This doc is about **device / image** portability (how many OS images cover how many phones). For
> **user-data** portability — moving a user's data **in** from another phone, **out** to another, and
> **cold-backup / self-host** with no fee — see [data-portability.md](data-portability.md).

How many Diaspore OS images must we build to cover most devices? **Not one per phone (that's hundreds
and unsustainable). A handful** — because Project Treble lets one image span many devices, and because
almost all of Diaspore's value is device-agnostic userspace.

## TL;DR

- **~1–4 GSI images** (mostly split by CPU arch) deliver the **Diaspore experience** (amnesiac, roaming,
  blind-login chooser) to **most Treble devices** — best-effort hardware, bootloader unlocked.
- **A handful of flagship devices** (FP3 + select Pixels) get the **full** Diaspore including **locked
  verified boot under our own key**.
- Because Diaspore sits **on top of LineageOS**, any of its ~200 device trees is a **low-marginal-cost**
  per-device build for the long tail.

The expensive, polished part (locked verified boot under our key) is deliberately a *small* set.

## Why it's not one-image-per-device: Treble / GSI

Project Treble (Android 8+) split the OS into a **device-agnostic `system` image** + a **device-specific
`vendor` partition** (the OEM's). A single **Generic System Image (GSI)** boots on *any* Treble-compliant
device; the vendor partition handles the hardware. That collapses "hundreds of device images" into "a few
system images."

## Diaspore is mostly userspace → GSI-shippable

The Diaspore secret sauce lives almost entirely in **Android userspace**, not in per-device boot code:

- the **roaming agent**, the **blind-login chooser**, the **app model**, the **content-addressed store**
  client — all userspace;
- the **boot/shutdown hooks** (P3.0) live in **`/system/etc/init`**, not a per-device `boot.img`;
- the **amnesiac `/data`** (wipe-on-power-off) can be a system-side init/fstab mechanism.

**Design implication:** to maximize GSI reach, keep Diaspore's footprint in **`/system`** and avoid
per-device `boot.img`/`init_boot` modifications where possible. The M2 stage-1 init interposition was one
way to hook early boot, but for the GSI tier we prefer system-side mechanisms so the device keeps its
**stock** `boot.img`/`vendor`. Per-device boot mods + a custom-key `vbmeta` are then needed **only** for
the flagship locked-verified-boot tier (below).

## The variant axes (few)

| Axis | Values | Note |
|---|---|---|
| CPU arch | **arm64** (≈ all modern phones), arm32 (legacy), x86_64 (rare) | arm64 alone covers the majority |
| Partition scheme | A/B vs A-only | a couple of GSI flavors |
| Flavor | vanilla only | Diaspore is inherently de-Googled — no gapps split |

→ **arm64-A/B + arm64-A-only + an arm32 fallback ≈ 2–3 images** cover the overwhelming majority.

## The catches (why not literally "1 image, every phone")

1. **GSI = best-effort hardware.** Generic HALs → camera / fingerprint / modem can be imperfect on some
   devices. "Boots + core features" ≠ "polished daily driver everywhere."
2. **Locked verified boot *under our key* is not GSI-universal.** It needs `avb_custom_key` + bootloader
   **relock**, supported only on some devices (Pixel, Fairphone…). A typical GSI flash leaves the
   bootloader **unlocked/orange**. So the *full* Diaspore security posture is a **flagship** thing.
3. **Non-Treble / very old devices** need the classic per-device model.

## The central tradeoff: reach vs verified boot

```
 GSI  ───────────────────────────────►  broad reach, best-effort HW, UNLOCKED bootloader
                                         (amnesiac + roaming + chooser all work)
 per-device flagship  ──────────────►   curated few, polished HW, LOCKED verified boot under OUR key
                                         (the full security posture: yellow/locked)
```

The amnesiac + roaming + chooser **experience** scales cheaply via GSI. The **locked verified boot under
our key** does not scale — it's per-device and bootloader-dependent — so it's intentionally a short list.

## The tiered strategy

| Tier | Build | Coverage | Security posture |
|---|---|---|---|
| **GSI** | ~1–4 images (by arch) | most Treble devices | unlocked (orange); amnesiac+roaming still work |
| **Flagship** | a handful (FP3 + select Pixels) | curated list | **locked verified boot under our key** (yellow), polished |
| **Ride LineageOS** | inherit ~200 device trees + layer our sauce | long tail, opt-in | per-device; verified-boot depends on the device |

## Device candidacy checklist

- **GSI tier:** Treble-compliant + unlockable bootloader + arm64 (usually). That's most modern phones.
- **Flagship tier (full Diaspore):** also needs **`avb_custom_key` + relock** support (Pixel, Fairphone)
  for locked verified boot under the Diaspore key, plus an official LineageOS device tree for polish.

## Build & maintenance cost

- **GSI:** a few CI targets → cheap to build + security-update. The scale play.
- **Flagship:** a few full device builds → moderate, hand-curated.
- **LineageOS ride:** automatable (inherit their trees), but each device is still a build config + needs
  security-update cadence — so "support all ~200" is a real ops commitment, not free.

## Recommendation / roadmap fit

1. **Now (Phase 4):** one flagship pilot end-to-end — **Fairphone 3** (Tier-2): full Diaspore + yellow
   locked verified boot. Prove the whole loop on real hardware.
2. **Next:** add a **GSI build** (Tier-1) for broad reach — the amnesiac/roaming experience on most
   Treble devices (unlocked). This is where "support most devices" actually comes from.
3. **Then:** a couple more flagships (a popular Pixel) and, optionally, automated per-device builds for
   select LineageOS devices.

So: **don't plan for hundreds of hand-built images.** Plan for **a few GSIs (reach) + a short flagship
list (full security) + optional LineageOS-ride builds (long tail).**
