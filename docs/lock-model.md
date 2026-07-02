# Lock model — protecting a live session when the phone is left unattended

Status: **design (2026-06-19); Layer 1 + Layer 2 implemented and verified on FP3 (2026-06-23), PR #120.**
Core feature (both editions). Decision (Chester): **re-auth to resume + auto-logoff backstop.** Complements
the power-off wipe; lives in `core/` (chooser + agent), so Diaspore and Endospore inherit it. **⚠️ L3
verification corrected a central assumption — screen-off does NOT FBE-lock CE; see
[Verified on FP3](#verified-on-fp3-2026-06-23).**

## The gap this closes

Two "unattended" states exist; only one was handled:

- **Power off** → seal + **wipe** → blank gate. ✅ (the core amnesiac behaviour)
- **Screen lock, still powered** → *nothing*. ❌ Boot deliberately runs with the **keyguard disabled**
  (`locksettings set-disabled true`) so the **gate is the sole auth** and there's no lockscreen flash before
  it. The side effect: mid-session there is **no lock at all** — wake → straight to the launcher. Worse, with
  no keyguard credential the roamed user's FBE **CE storage stays unlocked the whole time the phone is on**,
  so a "locked" phone is exposed both at the UI *and* at rest.

## Objective

When the screen is off mid-session: (1) require re-authentication to resume (a UI gate); (2) if the phone
is left unattended past a bound, **revert to amnesiac** automatically. Without reintroducing a lockscreen
flash at boot. **(The original goal of `/data` *encrypted at rest while locked* proved unattainable via the
keyguard — CE stays warm while the user runs; see [Verified on FP3](#verified-on-fp3-2026-06-23). At-rest
protection therefore rests on objective (2), the auto-logoff.)**

## Design

### Layer 1 — session-scoped keyguard (re-auth to resume)
- The keyguard is **session-scoped**, not global: the device-owner chooser **sets a lock credential on
  login** and **clears it on logoff/shutdown**. A fresh boot therefore still has no keyguard → gate is the
  boot auth, no flash. This is what reconciles "no boot lockscreen" with "mid-session lock."
- A correct unlock resumes the **warm** session (no re-restore) — verified on FP3. ⚠️ **Correction (L3):**
  the assumption that a credential makes Android **FBE-lock CE on screen-off** is **false** on standard FBE —
  CE is evicted only when the user is **stopped** (logoff/power-off), not on screen-off, so a locked-but-
  running session stays `RUNNING_UNLOCKED` (CE warm). The keyguard is a **UI re-auth gate, not at-rest
  encryption**; see [Verified on FP3](#verified-on-fp3-2026-06-23).
- **Credential = the profile passphrase by default** (one secret, strongest). **Optional roamed session
  PIN** for convenience — safe to keep short on **Endospore**, where the Titan M2 secure element
  hardware-throttles brute force (on the FP3's TEE a short PIN is weaker, so default to the passphrase).
  Biometric unlock is a later convenience layer on top.

### Layer 2 — auto-logoff backstop (revert to amnesiac)
- **Idle timeout:** screen locked + inactive ≥ T (configurable in gate→Settings, default **1 h** — strict-amnesiac
  means every login re-downloads the working set, so a longer default trades fewer re-downloads for a larger warm-
  CE window; high-security profiles can pick 2 min) → trigger
  the existing **logoff** (seal + wipe → gate). Uses device-owner `setMaximumTimeToLock` + an idle watcher
  in the chooser's persistent process (same place as the reap/sync watchers).
- **Failed attempts:** N wrong unlocks (`AdminReceiver.onPasswordFailed`) → logoff. Note **logoff, not a
  factory reset** — we keep the device's provisioning/store config; reverting to the amnesiac gate is the
  goal.
- The **continuous sync** means an auto-logoff never strands data (a recent seal already exists; see the seal
  cadence in [storage.md](storage.md)).

### Per-profile policy (later)
Hidden/duress or high-security profiles can opt into **lock = immediate logoff** or a shorter timeout.

## Implementation hooks (chooser = device owner)
- **Login** (`handleLogin` / `markRoamSession`): set the lock credential (DPM
  `resetPasswordWithToken`, device-owner with an enrolled reset token) + `setMaximumTimeToLock` +
  failed-attempt limit.
- **Screen-off**: Android FBE-locks CE storage automatically once a credential exists.
- **Wake**: standard keyguard prompts; correct credential resumes the session.
- **Idle/failed backstop**: chooser watcher → trigger `LOGOUT` (the existing seal+reap flow).
- **Logoff/shutdown**: clear the lock credential so the next boot → gate, no keyguard.

## Verified on FP3 (2026-06-23)

Layers 1 and 2 are **implemented and verified on hardware** (PR #120). L3 tested the at-rest assumption and
**disproved it** — the key finding of this pass.

**L1 — session-scoped keyguard (done).** Credential = the profile passphrase, set via
`resetPasswordWithToken` running **as the roamed user** (the API ignores a cross-user context, so a
`CredentialService` is started *into* the roamed user via `startServiceAsUser`; it enrolls a reset token —
which activates with no confirm on the fresh ephemeral user — and sets the credential). *Timing matters:*
enrolling a credential **locks the screen immediately**, so setting it at login dumped the just-authenticated
user onto a lockscreen (double passphrase). Fix: the gate **arms a one-shot `ACTION_SCREEN_OFF` receiver** and
sets the credential on the **first screen-off** (while the screen is already off) → **login lands on home**,
and the keyguard only appears once the user themselves sleeps the phone. Verified: login → home; screen-off →
wake → passphrase → unlocks.

**L2 — clear on logoff (done, automatically).** No explicit "clear credential" is needed: the roamed
identity is an **ephemeral** Android user, so logoff/power-off **reaps the whole user**, destroying its
credential (and CE key) with it. Verified: after logoff only user 0 remains; a fresh login starts
`CredentialType = NONE` — **no cross-session credential leak**. (The one-shot armer is also cancelled on the
logoff reap so it can't fire against a reaped user.)

**L3 — at-rest-while-locked: CORRECTION.** The spec assumed a credential makes Android **FBE-lock CE on
screen-off**, giving "`/data` encrypted at rest while locked (forensic-grade)." **This is false on standard
FBE.** Verified on the FP3: with the keyguard showing (`isKeyguardShowing=true`), the roamed user is still
`RUNNING_UNLOCKED` — its **CE storage stays decrypted (warm)** the entire time the phone is on. CE is evicted
**only when the user is stopped** (logoff / power-off / reboot). Therefore:

- The keyguard is a **UI re-authentication gate**, *not* at-rest encryption. A stolen **locked** phone still
  has the session's `/data` decrypted on disk until it is powered off.
- **Warm resume** is the upside: unlocking returns to the *same live session* instantly (apps + state intact,
  no re-restore) — verified.
- **Layer 2 / L4 (auto-logoff) is therefore the real at-rest guarantee** for an unattended locked phone, not
  a convenience. Its idle timeout directly **bounds the warm-CE exposure window**; a short timeout (or
  `lock = immediate logoff` for high-security profiles) is what actually delivers the amnesiac property when
  the phone is left locked. This raises L4's priority.

**L4 — auto-logoff backstop (done + verified).** The real at-rest guarantee (above). Two triggers, both
**reaping** the ephemeral session (seal + crypto-shred → blank gate) while keeping the device's
provisioning/store config — it is a logoff, **not** a factory reset:

- **Idle timeout** — the user-0 (persistent gate) process arms an **exact** alarm on `ACTION_SCREEN_OFF`
  (`setExactAndAllowWhileIdle` + the `USE_EXACT_ALARM` permission, so Doze can't defer it) and cancels it on
  `USER_PRESENT` (unlock). On fire it logs off **headlessly** — the user-0 receiver sends `LOGOUT` to the
  daemon directly, because launching the *locked* roamed UI cross-user is deferred by Android until unlock (so
  an attacker who never unlocks would otherwise never be logged off). A brief screen-wake from the gate
  Activity repaints the gate (the switch happens with the screen off). The timer counts from the **first**
  screen-off after the last unlock — a glance / notification doesn't reset it (#29).
- **Failed attempts** — `AdminReceiver.onPasswordFailed` (the chooser is profile owner of the roamed user) →
  after N wrong unlocks → auto-logoff. Requires `<watch-login/>` in `device_admin.xml`. **Not**
  `setMaximumFailedPasswordsForWipe` (that factory-resets the device).
- Verified on FP3: both triggers seal + reap to the gate; the exact alarm fires on time even in Doze.

**L5 — per-profile idle timeout (done + verified).** The idle timeout is a **per-profile** setting that roams
with the identity (like tz/locale) through `nowhere-prefs` (`idle=<min>`; `0` = Never; unset = the 1-h
default), chosen in the Profile screen (2 / 15 / 30 min, 1 h, Never) and read by the gate on login. Chooser-only — the prefs file roams
whole, so no agent/roamd change. A high-security profile can pick 2 min (small warm-CE window), a relaxed one
30 min or Never. Verified on FP3: `idle=2` roamed → the alarm armed for exactly 2 min and fired after 2 min of
true idle.

**#3 — resumable cold-lock + hard-wipe thresholds (done; DIA-20260626-03).** Under the resumable-cold model
([resumable-session.md](resumable-session.md)) the idle action is no longer a seal+wipe logoff but a
**cold-lock** (stop the user → CE key evicted → ciphertext at rest, resumable via "Welcome back"). This slice
sets the **two-stage** idle backstop:
- **Cold-lock at 15 min** — the `IDLE_LOGOFF_MS` default dropped 1 h → 15 min (per-profile `idle=<min>`,
  `0` = Never). This is the first stage: the session is parked, resumable, no data leaves the device.
- **Hard-wipe at 12 h** — a SECOND alarm armed *at* cold-lock (`armWipeWatcher` → `ColdWipeReceiver`); if the
  cold-locked session is still un-resumed after the bound, it's removed (`coldWipe` → `doRemove`) — the same
  amnesiac removal a power-off boot-wipe does, just on a timer. Guarded by the daemon's `.coldlock` marker (a
  resumed session is never wiped; uid must match) and the marker is dropped via the new `CLEAR-COLDLOCK` daemon
  verb. Per-profile `wipe=<hours>` (`0` = Never; unset = 12 h). **No data loss beyond the existing amnesiac
  contract:** cold-lock doesn't seal (the continuous sync already backed it up), so the wipe loses at most the
  post-last-sync delta — identical to an unclean power-off.

## Risks / open questions (validate on the FP3 with a live session)
- **Per-session credential mechanics**: ✅ resolved on FP3 (see [Verified on FP3](#verified-on-fp3-2026-06-23))
  — `resetPasswordWithToken` works as device owner with an enrolled token, set on first screen-off and cleared
  by the ephemeral reap. ❌ CE-key eviction does **not** trigger on screen-off (only on user-stop), so the
  at-rest goal moves to the auto-logoff backstop.
- **Passphrase-as-credential UX**: typing a long passphrase to unlock is tedious → the optional PIN matters;
  decide whether the PIN roams as a profile pref.
- **FP3 vs Endospore strength**: TEE-only throttling vs the Pixel secure element — the short-PIN option is
  only really safe on Endospore.
- Keep the boot keyguard-off intact; the session-scoped credential must be fully cleared on every exit.

## Implementation plan (next up — slices)

Status: **planned 2026-06-22**, sequenced after the Endospore `user` relock re-validation. The design above
is settled; this is the execution breakdown (tracked as tasks L1–L5, + the SE throttle **S1** in
[se-binding.md](se-binding.md)). **Key insight that drives the whole thing:** setting a *real* Android lock
credential on login is the single hinge — it makes Android FBE-evict the CE key on screen-off (the
encrypted-at-rest win) *and* routes unlock through the Gatekeeper/Weaver hardware throttle for free; the
per-session set/clear is what keeps the boot gate flash-free.

- **L1+L2 — session-scoped keyguard (the foundation).** Device-owner chooser enrolls a
  `setResetPasswordToken` at provision / first login; `resetPasswordWithToken(credential)` on login,
  **clear on logoff/shutdown**. Fresh boot → no keyguard → gate is the sole boot auth (`nowhere_gate.sh`
  re-asserts `set-disabled` as a backstop). *Resolve first:* whether the reset token needs a one-time
  on-device activation on these locked-down builds, **and** a boot-time stale-credential recovery — a
  stranded credential = keyguard-before-gate, a visible regression of the "gate is the only auth" property.
- **L3 — CE-at-rest-while-locked + warm resume (the payoff).** Verify on-device that screen-off evicts the
  CE key (`/data` encrypted at rest while locked) and a correct unlock resumes the *warm* session — no
  re-restore.
- **L4 — auto-logoff backstop.** `setMaximumTimeToLock` + an idle watcher (locked + idle ≥ T, default
  ~15 min) and `onPasswordFailed` (N wrong) → the existing seal+wipe→gate **logoff** (not a factory reset).
  Reuses the chooser's reap/sync watcher; continuous sync means it never strands data.
- **L5 — credential UX.** Default = profile passphrase; optional short **session PIN** that roams as a
  profile pref (safe-short only on Endospore's Titan M2; FP3 defaults to the passphrase). Gate→Settings
  toggles for the PIN + idle timeout. Biometrics are a later convenience layer.
- **S1 (Endospore) — SE Weaver throttle.** Bind the StrongBox `se_secret` release to *this* lock credential
  so on-device guessing is hardware-throttled — the complement to the already-closed offline path. Lives in
  [se-binding.md](se-binding.md); depends on L1+L2 (the credential is the throttle anchor).

**Order:** L1+L2 → L3 → L4 → L5 → S1. L1–L5 are core (both editions inherit); S1 is the Endospore-only
hardware delta.

**Build-first platform:** build + validate **L1–L5 on the FP3 (Diaspore) first** — the more mature platform,
and where the lock screen was originally scoped (the "validate on the FP3" note above). Endospore then
inherits L1–L5 **unchanged from `core/`** and adds **S1** on top. The FP3 has **no StrongBox**, so it relies
on TEE-Gatekeeper keyguard throttling and defaults to the passphrase (L5); only the `se_secret`-bound
throttle (S1) is Pixel-only. So this plan is *not* Endospore-specific — Diaspore gets the full lock screen;
Endospore gets the same lock screen plus the secure-element throttle.

**Decisions to settle at L-start:** idle-timeout default, failed-attempt N before auto-logoff,
passphrase-only vs. offering the PIN from day one, and whether S1 ships *with* the lock screen or as a
fast-follow.

## Exit
Lock the phone mid-session → wake **challenges for the passphrase** → re-auth resumes the *warm* session (CE
stays warm while locked — the keyguard is a UI gate, **not** at-rest encryption); leave it locked + idle past
the (per-profile) timeout, or fail N unlocks → it seals, **crypto-shreds**, and returns to the blank gate.
**L1–L5 all implemented and verified on the FP3** (see [Verified on FP3](#verified-on-fp3-2026-06-23)).
