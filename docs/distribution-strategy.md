# Diaspore / Endospore — Distribution strategy: SKUs, the install bottleneck, and onboarding

Status: design · 2026-06-21 · pairs with billing-model.md, commercialize.md,
[enrollment.md](enrollment.md), [roadmap.md](roadmap.md) (Phase 2)

How people get the OS onto a device and start paying. Captures the product split, an honest read of the
install bottleneck, and the highest-leverage thing to fix.

## The reframe: one product, two acquisition channels

It is tempting to think of three products. It is clearer to think of **one product and two funnels**:

- **The product is the token** — the roaming-store subscription / prepaid credits (billing-model.md).
  It is the recurring revenue and the thing a customer actually keeps. Everyone ends up here.
- **Hardware** and **bring-your-own-phone (OS download)** are just two *ways to get the OS onto a device* so
  a token can be used.

So the catalogue is:

| SKU | What it is | Role | Friction |
|-----|-----------|------|----------|
| **i. Hardware + token** | pre-flashed, pre-provisioned device + bundled credits | the **easy / trusted** funnel; premium price; we carry the flashing + (some) inventory | ~zero for the buyer |
| **ii. Token** | credits only, for a device that already runs the OS | the **revenue engine**; renewals + new credits | billing only |
| **iii. BYO phone (OS) + token** | free/cheap OS download for a *supported* phone + credits | the **scale** funnel; no hardware logistics | install + onboarding |

This split is sound and matches the market: NitroPhone (pre-installed GrapheneOS) and Murena (pre-installed
/e/OS) both prove that **pre-flashed hardware sells precisely because security buyers would rather not trust
their own flashing.** BYO is the long tail, not the core.

## The install bottleneck is two bottlenecks — do not conflate them

### Layer 1 — flashing the OS (partly unfixable)

- **The lever is a branded WebUSB installer** — flash from the browser, guided, no CLI. This is exactly what
  GrapheneOS's web installer does; the GrapheneOS factory `flash-all.sh` we ship literally says *"using the
  web installer is strongly recommended."* That is the model for the **Endospore (Pixel)** edition.
- **Hard floor we cannot remove:** bootloader unlock requires the OEM's "OEM unlocking" toggle, and most
  phones (Samsung, carrier-locked, etc.) cannot be unlocked at all. So **"bring your phone" really means
  "bring a *supported* phone"** — Pixels (Endospore) and Fairphone (Diaspore). Be clear-eyed: that is a
  narrow slice of the market, not all of it.
- **Bridge between (i) and (iii):** a *"mail us your Pixel / drop by a partner shop, we flash it"* service.
  Removes the scary part without us carrying full hardware inventory. Repair-shop partners scale this.
- **UX wart to budget for:** the **yellow "loading a different operating system" boot screen** on every
  power-on — the unavoidable cost of verified boot under our own AVB key. GrapheneOS users tolerate it;
  mass-market may not. It needs a line in the messaging ("this warning means *your* key is in control"),
  not an engineering fix.

### Layer 2 — onboarding (Wi-Fi + store + identity) — mostly already solved

**The painful five-field store-config entry is a dev-mode artifact, not the product.** A customer should
never type a raw S3 endpoint / region / bucket / access key / secret key. In the **managed-store** model
(Phase 2):

- **The token *is* the store access.** The customer **scans a QR / enters a short token code**; the device
  pulls its (scoped, revocable) store config over the **`discover` / `publish-discovery`** path that already
  exists in the agent (see [enrollment.md](enrollment.md), [storage.md](storage.md)). No S3 credentials ever
  touch the user.
- That turns onboarding from "five error-prone fields with no feedback" into **"scan the card that came with
  your token."** It is **blind-compatible**: discovery fetches client-encrypted config, so the QR only points
  at ciphertext — it leaks nothing even if photographed.
- It is a **strength of the architecture, not a weakness**: the same primitive serves all three SKUs,
  including the hardware one (a factory device can ship pre-pointed at a discovery handle and bind to a token
  on first run).

## Recommendation

1. **Treat the token as the product**; hardware and the web-installer BYO path are two acquisition channels
   into it; "we-flash-it-for-you" is the bridge between them.
2. **Prioritise token-QR onboarding (Layer 2) over flashing UX (Layer 1).** It is higher-leverage — it helps
   *all three* SKUs, it is mostly software whose primitives (`discover` / blind store-config) already exist,
   and it removes the worst first impression. Flashing ease has a hard ceiling (device support + unlock
   gates); onboarding ease does not.
3. **Lead with pre-flashed hardware** as the trusted funnel; ship the **WebUSB installer** for the supported
   Pixels as the scale funnel; keep BYO honestly scoped to supported devices.

## Sequencing / roadmap placement

This is **Phase 2 (Commercialize)** material — it sits on top of the managed store + token gateway in
commercialize.md and the credit/token economics in billing-model.md.
Concretely:

- **Token-QR onboarding** depends on Phase-1 **Tier 2 (store config / discovery)** — already done — plus the
  Phase-2 gateway issuing scoped, discoverable store handles against a redeemed token. Build it *with* the
  gateway, not after.
- **WebUSB installer** is an independent storefront workstream (browser tooling + per-device factory images);
  it can proceed in parallel and is reusable across Endospore (Pixel) editions.
- **"We-flash-it" / partner program** is an operations play, gated on demand — defer until the storefront
  shows BYO interest.
