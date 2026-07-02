# Endospore — Spec: GrapheneOS / Pixel "Secure Edition"

Status: **E.0–E.4 DONE on the Pixel 7a (`lynx`)** — locked verified boot under our AVB key, blind-login gate,
full roaming + power-off wipe, enforcing `endospore_priv`, OTA via our server, branding, `nowhere*` rename
(all proven on HW, ~2026-06-21). **Now: E.5+ — catch up to current `core/`, reach UX parity with Diaspore,
productionize the build/OTA, and expand devices (decided 2026-06-30; see "Beyond E.4" below).** Product-roadmap
**Phase 3** (see [roadmap.md](roadmap.md)). Pairs with restructure.md (the `core/` +
`editions/` topology), [device-onboarding.md](device-onboarding.md) (the per-device gate), and
[portability.md](portability.md).

> Endospore is a **second, higher-assurance edition** of the same amnesiac roaming phone — Pixel + a
> **GrapheneOS-derived** build — riding the *same* roaming store + prepaid credits as Diaspore. It is **not**
> a rewrite: the security-critical code (`core/agent`, `core/chooser`, `core/vendor-common`) is shared and
> already proven on the FP3. Endospore is a **port + build-integration + verified-boot** effort.

## Objective / exit

An Endospore device that, on a real Pixel 6/7:
1. boots a **locked, verified** GrapheneOS-derived OS under **our AVB key** (relocked bootloader);
2. shows the **blind-login chooser** (profile + passphrase, no list) — `core/chooser`;
3. **restores that profile** end-to-end and wipes on power-off — `core/agent`, exactly as on the FP3;
4. **updates** via an OTA we serve (Android A/B), not GrapheneOS's update server;
5. binds keys to the **Titan M2 secure element** (StrongBox) — the assurance step up over the FP3.

## Why Pixel + GrapheneOS (what this edition buys)

The FP3 (Diaspore) tops out at **custom-key yellow** verified boot with **no secure element**. Endospore's
entire reason to exist is higher assurance for the security/vertical market:

- **Secure element (Titan M2):** hardware-bound key custody (StrongBox/Weaver), brute-force throttling on
  the passphrase, insider-resistant key storage — none of which the FP3 has.
- **Locked verified boot, properly:** Pixels let you relock the bootloader against a **custom AVB key**, so
  verified boot + rollback protection are genuinely enforced (the boot screen shows **yellow** with our key
  fingerprint — green is Google-key-only, but the lock + rollback + SE are real). This is the path
  GrapheneOS itself uses.
- **GrapheneOS hardening:** `hardened_malloc`, exploit mitigations, hardened sepolicy, network/sensor
  permission toggles — defense-in-depth that complements (doesn't replace) our amnesiac/roaming thesis.

What's **reused unchanged** (the point of the restructure): `core/agent` (roaming engine), `core/chooser`
(blind-login gate), and most of `core/vendor-common` (sepolicy, init `.rc`, `bin/` workers, overlays,
default-permissions). Endospore supplies only the **edition delta**: GrapheneOS/Pixel build glue + device
config + any sepolicy/overlay adjustments GrapheneOS's stricter base demands.

## Key differences from Diaspore (why it's its own phase)

1. **Base OS + build system.** GrapheneOS is hardened AOSP built with **its own scripts** (`grapheneos.org/build`),
   `adevtool` for Pixel vendor blobs, and per-device release signing — *not* LineageOS `brunch`. Endospore
   needs its own build glue (`editions/endospore/build/stage-vendor.sh` against the GrapheneOS tree).
2. **Relock with our key.** Unlike the FP3, we `fastboot flash avb_custom_key` + `fastboot flashing lock`
   → genuinely locked, rollback-protected, SE-active. Same mechanism GrapheneOS ships with.
3. **Stricter sepolicy.** GrapheneOS's hardened policy will reject more than LineageOS did → expect more
   sepolicy work to land the agent (su:s0 worker), chooser, and the amnesiac `/data` flow.
4. **Update cadence is a standing cost.** GrapheneOS ships security updates **very frequently**. A modified
   derivative must track that cadence (rebase + rebuild + re-sign + OTA) or fall behind on the exact
   hardening that justifies the edition. This is the biggest *ongoing* commitment, not a one-time build.
5. **Naming / credit.** GrapheneOS does not support out-of-tree modifications; Endospore must present as
   **"based on / derived from GrapheneOS,"** never *as* GrapheneOS, and credit upstream.

## Milestone ladder (mirrors the proven Diaspore P4 ladder)

```
E.0 scaffold + spec ──→ E.1 build+flash vanilla GrapheneOS ──→ E.2 integrate core/ ──→ E.3 relock+SE ──→ E.4 OTA
       (no HW)                  (needs Pixel)                      (roam on Graphene)     (verified boot)
```

### E.0 — Scaffold + spec  *(no hardware — this slice)*
Create `editions/endospore/` (mirrors `editions/diaspore/`) and this spec. No build yet.
**Exit:** the edition exists in the tree; the ladder + build env + relock story are written down and reviewable.

### E.1 — Build + flash vanilla GrapheneOS  *(needs the Pixel)*
Stand up the GrapheneOS build for the chosen device, generate **our** signing + AVB keys, sign, flash, and
**relock** with the custom key — *no Diaspore changes yet*. This proves the build→sign→flash→relock pipeline
in isolation (the riskiest new ground).
**Exit:** a vanilla GrapheneOS-derived build, signed by us, boots **locked (yellow, our key)** on the Pixel;
factory-reset + OTA-self-update work on the stock build.

### E.2 — Integrate `core/`
Add an `endospore` `stage-vendor` that assembles the GrapheneOS-side vendor pieces from `core/vendor-common`
+ the edition delta + the freshly built `core/agent`; bake agent + chooser into the image; port the
amnesiac `/data` + boot/shutdown flow; resolve GrapheneOS sepolicy denials.
**Exit:** blind-login → full roaming restore → usable → wipe-on-power-off, **on the Pixel**, same loop as the
FP3. *(This is "something working.")*

### E.3 — Verified boot relocked under our key + SE binding
Confirm relock holds with the integrated build; bind the passphrase-derived/data keys to **StrongBox**
(Titan M2) so key custody is hardware-backed and throttled.
**Exit:** locked verified boot with the integrated OS; keys are SE-bound; tamper → boot refuses.

### E.4 — OTA via our server
Repoint the Android A/B updater at an OTA we serve (reuse the Diaspore content-addressed OTA work, or adapt
GrapheneOS's `Updater` to our endpoint), so devices update without GrapheneOS's server.
**Exit:** publish v2 → device pulls + A/B-applies + verifies + reboots into v2, under our key.

## Device matrix (Pixel 6/7 — Tensor)

| Model | Codename | SoC | Notes |
|---|---|---|---|
| Pixel 6 | `oriole` | Tensor G1 | cheap, common dev target |
| Pixel 6 Pro | `raven` | Tensor G1 | |
| Pixel 6a | `bluejay` | Tensor G1 | cheapest |
| Pixel 7 | `panther` | Tensor G2 | newer, longer support window |
| Pixel 7 Pro | `cheetah` | Tensor G2 | |
| Pixel 7a | `lynx` | Tensor G2 | cheapest of the G2s |

**Pin the exact unit at E.1** — the codename selects the `adevtool` extraction + the build target. Any of
these works; a **Pixel 7 / 7a (`panther`/`lynx`)** has the longer remaining GrapheneOS support window if buying fresh.

## Build environment

- GrapheneOS build needs a Linux host with **lots of RAM (32 GB+ recommended) + disk (~400 GB)** + many
  cores — comparable to AOSP. **Check the build-VM capacity**: it carries the LineageOS tree
  (`/mnt/build/lineage`); GrapheneOS needs its **own large tree** — likely a separate checkout and possibly
  a bigger disk. Serialize OS builds (one `out/`).
- `adevtool` extracts the proprietary Pixel vendor files per device.
- **Keys stay off the repo** (build VM / CI secrets), backed up like the existing Diaspore AVB keys at
  `C:\Linnae\keys`. Endospore needs its **own** releasekey/platform/AVB/OTA keys — do not reuse Diaspore's.
- Confirm all specifics against the current `grapheneos.org/build` at execution (versions drift fast).

## Risks / open questions

- **Maintenance cadence** (the big one): tracking GrapheneOS's frequent security releases with our
  modifications is an ongoing rebase+rebuild+re-sign+OTA cost. Budget for it or the edition decays.
- **Remote attestation:** hardware key attestation reveals a **custom (non-Google, non-GrapheneOS) AVB key**,
  so third-party "is this stock?" attestation won't pass as Google/GrapheneOS. Local hardware key binding
  (StrongBox) is unaffected. Decide what the "secure edition" claims about attestation.
- **sepolicy strictness:** GrapheneOS may reject the agent's su:s0 worker / device-owner / amnesiac-`/data`
  patterns that LineageOS allowed — likely the bulk of E.2 effort.
- **Amnesiac `/data` on GrapheneOS:** verify the stage-1 init / FBE / wipe-on-shutdown approach ports to
  GrapheneOS's init + encryption setup.
- **Sequencing:** roadmap puts Endospore *after* Commercialize for a reason (prove revenue first). This
  kickoff is exploratory; the Phase-2 deploy (nowhere-cloud PR #8) is parked, not abandoned.

## Beyond E.4 — parity, productionization, multi-device  *(decided 2026-06-30, Chester)*

E.0–E.4 are done on `lynx`, but Endospore is frozen at ~June-21 `core/` while Diaspore has advanced ~10 days
(lock model L1–L5 + resumable-cold, cap-mode + free-tier + subscriptions, seal-perf, throwaway, gate UX). The
goal: **bring Endospore to "Diaspore as it is now," with a matching UX**, then expand devices.

### E.5 — Catch up to current `core/` + re-validate on `lynx`
- **E.5a** Re-sync the GrapheneOS tree to a current release + rebuild `lynx` off current `core/`; re-prove the
  base loop (gate → roam → enforce → wipe).
- **E.5b** Lock model on GrapheneOS — L1–L5 + **resumable-cold** (cold-lock / resume / 15 min–12 h wipe);
  validate FBE-lock/resume on Graphene's encryption. The **SE-throttled credential** is the distinguisher here.
- **E.5c** Managed store — cap-mode + blind-token billing + **free tier** + **subscriptions**, validated on
  `lynx` against the live gateway (same on-device flow as the FP3).
- **E.5d** **UX parity** (decided: identical experience to Diaspore) — default apps + two-page home +
  timezone/locale + Wi-Fi onboarding + gate-settings gear, as **GrapheneOS-side overlays** (Graphene has no
  LineageParts/SetupWizard/Trebuchet to RRO, so this is real edition work, not a copy).

### E.6 — Distinguishing security
- **E.6a** SE-binding posture (#18): the offline-brute-force closure (E.3b, non-extractable StrongBox secret)
  stands; the **Weaver on-device-guessing throttle stays PARKED** under the passphrase-everywhere decision
  ([resumable-session.md](resumable-session.md)) — revisit only for a short-PIN-on-Endospore.
- **E.6b** **Attestation stance (#10)** — document what "secure edition" claims: a custom AVB key ≠ Google/
  GrapheneOS, so third-party "is-it-stock" attestation won't pass; local StrongBox binding is unaffected. Feeds
  the verify-the-architecture / storefront comparison.

### E.7 — Productionize build / release / OTA
- **E.7a** A repeatable Endospore pipeline mirroring Diaspore's `p4.x` (stage → build → sign → relock → OTA),
  scripted + documented (today it's ad-hoc E-phase scripts).
- **E.7b** Fix **#23** (OTA apply hangs on locked `lynx` — diagnose on userdebug).
- **E.7c** **Automated OS-update cadence — BOTH editions** (decided 2026-06-30): a mostly-automated
  rebase → rebuild → re-sign → OTA, on AOSP-monthly-security for both, **plus the extra GrapheneOS point
  releases** (Graphene ships multiple/month; manual won't keep up, and falling behind erodes the hardening that
  justifies the edition). LineageOS can stay ~monthly.
- **E.7d** Public mirror + CI for the `endospore` repo (deferred; like Diaspore's publish pipeline).

### Multi-device expansion (cross-edition)
Order (Chester 2026-06-30): **Pixel 10a** (Endospore, strongest) · **Fairphone 6** (Diaspore, FP3 successor) ·
**Moto G 2025** (Diaspore — ⚠️ **confirm custom-key relock FIRST**; most Motorolas can't, which drops the
verified-boot pillar to "reduced tier"). Each device is a bounded `devices/<codename>/` add — see the gate +
per-device checklist in **[device-onboarding.md](device-onboarding.md)**.

### Diaspore vs Endospore — the differentiators (for the storefront "which edition" page)
| Capability | Diaspore (LineageOS / FP3) | Endospore (GrapheneOS / Pixel) |
|---|---|---|
| Secure element | none — passphrase guarded by **software KDF only** (offline-brute-forceable) | **Titan M2** (StrongBox/Weaver): passphrase **hardware-throttled + non-extractable** → not offline-brute-forceable |
| Exploit hardening | stock AOSP allocator | `hardened_malloc`, **MTE** (Tensor), exploit mitigations, hardened sepolicy |
| Verified boot | custom-key relock → yellow (weaker hardware root) | custom-key relock → yellow on a **Titan-anchored** root of trust |
| Privacy controls | LineageOS basics | per-app network + sensor toggles, storage scopes, duress, auto-reboot |
| Wins instead | **breadth** (100s of devices), **ethics/repairability** (Fairphone), cost | higher assurance for the security/vertical market |

Both share the thesis (amnesiac roaming + verified boot under **our** key + managed store + zero-knowledge
billing). Endospore = that **+ hardware assurance**; Diaspore = that **+ reach + ethical hardware**.
