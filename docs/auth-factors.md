# Auth factors — convenience unlock for resuming a live session

Status: **design (2026-06-26).** Decision (Chester): on top of the passphrase, offer **convenience factors
(PIN / pattern, fingerprint) for the resume/unlock moment** — *not* the cold gate. The factor is a
**per-profile** setting, configured after login. Complements [lock-model.md](lock-model.md) (L1–L5) and
[resumable-session.md](resumable-session.md); the strong/persistent variant is the
[se-binding.md](se-binding.md) (Weaver) work.

## Scope: *which* unlock moment

There are two auth moments, with opposite constraints. This doc is about the **second** one only.

1. **Cold gate — blind login (amnesiac, nothing on the device).** Out of scope here, and unchangeable:
   at a fresh power-on the device has *zero knowledge* of the user until they type the passphrase
   (`key = Argon2id(pass, salt(name))` → the DK that unseals everything from the store). Stays passphrase,
   or the secure-element slot (`seDK`). See [identity-model.md](identity-model.md).
2. **Resume — session already live, powered on** (screen-off, or a cold-lock under the resumable model).
   This is the **common** moment — the resumable-cold model means many lock→resume cycles per boot, each
   currently costing a full passphrase re-type. This is where convenience factors fit.

## Core principle: a biometric is not a secret

A fingerprint isn't run through a KDF — Android stores a **template** in the TEE and uses a *match* to
release a keystore/gatekeeper-held secret. So a biometric can only ever **gate a secret that already exists
on the device**. At the cold gate there is no such secret (amnesiac), so biometrics can't bootstrap a key
there. Once a session is live the secret *does* exist (in memory / a keystore key), so a biometric — or a
short PIN — can release it to drive the resume. **The high-entropy passphrase therefore stays the root of
the identity; convenience factors only ever unlock a *resume*.**

## The factors

### Fingerprint — per-boot, on every device (incl. Pixel)
The template lives in `/data` (TEE-wrapped), and our amnesiac model wipes `/data` on power-off. We
deliberately do **not** persist biometric templates at rest (privacy; "nothing at rest" is the whole point).
So fingerprint is **re-enroll-once-per-boot everywhere** — a tap to enroll after the first login, then it
covers that power-cycle's resumes. It is never a "set once" factor, and Pixel does not change this.

### PIN / pattern — knowledge factor; within-boot everywhere, persistent only with a hardware throttle
A PIN/pattern is a *knowledge* factor: nothing is stored but the verification material, and the value lives
in the user's head — so the **value** roams for free ("my unlock is 1-3-7-9 everywhere"). Whether the
*mechanism* can persist across reboots hits one wall ↓.

### The entropy wall
A 4–6 digit PIN (or a 3×3 pattern) has tiny entropy. If the secret it protects sits at rest (in the store,
or in a file that survives), anyone holding that ciphertext can try all ~10⁴ PINs **offline** in seconds and
recover the key — which destroys the zero-knowledge guarantee. A short factor is only safe if **something
rate-limits the guesses in tamper-resistant hardware**. That throttle can live in exactly two places, and
that choice decides what "roam" means:

| Throttle location | Survives power-off? | Roams to a new device? |
|---|---|---|
| **The device's secure element** — Weaver/StrongBox on Pixel; FP3's gatekeeper handle is in `/data` (wiped) | Pixel: **yes**; FP3: no (within-a-boot only) | No — bound to that chip |
| **An attested server enclave** (Signal-PIN / "secure value recovery": a short PIN unlocks a high-entropy key behind a hardware-enforced ~10-try limit) | yes | **Yes — genuinely follows the user** |

## Device / secure-element matrix

It's a **chip/secure-element capability, not the Fairphone brand.**

| | Fingerprint | PIN/pattern (this boot) | PIN/pattern (persists across reboots) |
|---|---|---|---|
| **FP3** (Snapdragon 632, no StrongBox SE) | per-boot enroll | ✅ | ❌ |
| **FP4 / FP5** (newer Qualcomm, StrongBox SPU) | per-boot enroll | ✅ | **maybe** — needs per-device verification of Weaver / a rollback-protected counter |
| **Pixel** (Titan M + Weaver) | per-boot enroll | ✅ | ✅ (reference) |

## "Set every time" = once per *boot*, not per *unlock*

On FP3 the convenience factor is re-established once per power-cycle: type the passphrase once at the gate,
then the PIN/fingerprint covers **every** cold-lock resume for that whole power-cycle — and in the
resumable-cold model the phone stays on (cold-locked when idle) up to 12 h before it wipes. So the re-setup
is a roughly once-a-day-ish event tied to a full power cycle, **not** friction on every unlock. A
Weaver-class device removes even that.

## Where setup lives: the Profile

Auth-factor enrolment is **per-profile**, configured *inside a live session* (after the passphrase login
that bootstraps the secret). It belongs in the **Profile** management surface — the same place that already
holds the recovery code, store config, and delete-profile.

> ⚠️ **Profile is getting crowded** (recovery code · store · delete · now auth factors · and the future
> **token-usage / billing** view). Decision (Chester): **redesign the Profile surface together with the
> token-usage work** in Phase 2 (commercialize.md / billing-model.md)
> rather than bolting each new setting on. Until then, add the factor toggle as a minimal entry; don't invest
> in Profile IA yet.

## Roadmap mapping

- **Within-boot PIN / pattern + fingerprint enrol** → folds into **L5** ([lock-model.md](lock-model.md),
  task #17, "credential UX + optional roamed session PIN"). The simplest, device-agnostic win — removes the
  per-resume passphrase retype. **Recommended first.**
- **Persistent, hardware-throttled PIN** → **Weaver/StrongBox binding** (task #18,
  [se-binding.md](se-binding.md)). The "set once, survives reboots" experience — a **Secure-Edition
  (Pixel/Endospore) differentiator**. FP4/FP5 may inherit it pending the verification above.
- **Fully-roaming enclave PIN** (Signal-style SVR) → later, only if users demand a short factor that follows
  them across devices without a passphrase. Adds a trust-minimised (attested) server component + a hard
  "too many wrong tries → lock/destroy" rule.

## Open questions

- **Resume mechanism:** lean on Android keyguard's own biometric-unlocks-CE path, or keep our chooser-driven
  resume and release a biometric/PIN-bound passphrase ourselves (lean: the latter — keeps the custom resume
  flow we already built, easier to reason about).
- **FP4/FP5:** confirm whether their StrongBox SPU exposes Weaver or a usable rollback-protected counter.
- **Lockout policy:** tie wrong-PIN attempts into the existing L4 failed-attempt backstop → fall back to the
  amnesiac wipe (don't invent a second throttle).
- **Biometrics at rest:** recommendation is to **never** persist fingerprint templates across power-off
  (per-boot re-enrol everywhere), even where hardware could — keeps "nothing at rest" intact.
