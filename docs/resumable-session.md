# Resumable session — FBE-lock-on-idle (the missing middle state)

Status: **DECIDED 2026-06-25 (Chester) — adopt as the DEFAULT lock model; supersedes the strict-amnesiac +
1 h-idle interim.** Design raised 2026-06-24. Extends [lock-model.md](lock-model.md); pairs with
[storage.md](storage.md) and the **seal optimizers** (blob cache, PR #146 + content cache, held for this) that
make the now-rarer roam/backup seal fast. Implementation plan + the sharpened security framing below
(DIA-20260625-10).

## The problem: a two-state model forces a three-way tension

The lock model has only **two** session states:

- **Warm** (logged in): `/data` is decrypted in the running system — the **warm CE** (L3: screen-lock does NOT
  encrypt it). Instant to use.
- **Wiped** (logoff / idle-timeout / power-off): the **ephemeral** roamed user is reaped → `/data`
  crypto-shredded → the next login **re-downloads the full working set** from the store.

With no state in between, the **idle auto-logoff** — which is the real at-rest backstop, since a *locked* phone
otherwise keeps `/data` warm — forces a full re-download on every resume. That pins you to **two of three**:

1. **nothing kept on device** · 2. **auto-logoff (at-rest security)** · 3. **no re-download per login**

| Choice | Get | Give up |
|---|---|---|
| 1 + 2 (today) | nothing kept + at-rest security | re-download every login |
| 1 + 3 (`Never` idle) | nothing kept + no re-download | at-rest security (warm CE on a lost-but-on phone) |
| 2 + 3 (warm cache) | at-rest security + no re-download | "nothing kept" (persists encrypted chunks) |

## The fix: a third state — idle-locked, encrypted at rest, resumable

Insert a middle state between *warm* and *wiped*:

| State | Trigger | `/data` | Resume | Promise |
|---|---|---|---|---|
| **Active** | in use (idle < T) | decrypted (warm CE) | instant | — |
| **Idle-locked** | idle ≥ T | **FBE-encrypted at rest** (CE key evicted) | unlock passphrase → decrypt **in place**, *no re-download* | nothing **readable** kept |
| **Amnesiac** | explicit logoff / power-off (+ boot-wipe) | **deleted** (crypto-shred) | full re-download | nothing kept |

**Mechanism.** At idle T, **stop the roamed user** — which *evicts its CE key* and FBE-locks `/data` — but do
**not** delete it. Re-entry runs through the keyguard: enter the passphrase → the user starts → its CE key is
re-derived → `/data` decrypts **in place** → resume instantly (apps + state intact, no restore). An **explicit
logoff** (or a **boot-time wipe** after a power-off) deletes `/data` → amnesiac.

This **resolves the tension**:

- **Amnesiac on power-off** ✓ — the deliberate "I'm done" still wipes everything.
- **Encrypted at rest while idle-locked** ✓ — a lost-locked phone is FBE-encrypted, passphrase brute-force
  throttled by the TEE (Titan M2 on Endospore). Far better than the warm-CE exposure of `Never`.
- **No re-download for the common case** ✓ — you pay it only after a *real* power-off, not after every coffee
  break or overnight idle.

It is also exactly the "`/data` encrypted at rest while locked" that **L3 deemed unattainable via the
keyguard** — but L3's finding was narrowly that *screen-off* doesn't evict the CE key. An explicit
**idle-timeout user-stop does**. So we reach the L3 goal by triggering it deliberately on a timer rather than
hoping the keyguard does it on screen-off.

## Decision (2026-06-25, Chester): this is the DEFAULT, and it's a security *upgrade*

We keep **amnesiac roam**, but make the **resumable middle state the default** rather than strict wipe-on-idle:

- **Idle default → 12 h** (the resumable picker: 15 min / 1 h / 6 h / 12 h / 24 h / Never), replacing the
  strict-amnesiac **1 h**. A duress / high-security profile opts back into **wipe-on-idle** (today's behaviour)
  — so strict-amnesiac becomes the per-profile *override*, resumable-cold is the default.
- **Cold-encrypt-on-lock is mandatory, not optional** — it's the thing that keeps the 12 h claim honest.

**Why this is a security upgrade, not a concession.** Today the idle window is **warm** (CE decrypted behind a
UI keyguard — `lock-model.md` L3); the only mitigation is that the window is short (1 h). The resumable model
makes the window **cold** (CE key evicted, `/data` ciphertext). So **12 h-cold is strictly safer than 1 h-warm**
— a longer window of *encrypted* beats a shorter window of *decrypted*. We close the L3 warm-CE gap **and** get
the convenience; both axes improve.

**The amnesiac claim, stated precisely.** "Amnesiac" honestly means *a cold or powered-off device holds nothing
usable* — only ciphertext, with the key recoverable solely from the passphrase (never stored). Cold-key-eviction
delivers exactly that, so the claim holds at any idle length. The one honest softening vs strict instant-wipe:
the ciphertext is *present* while idle-locked, so the guarantee rests on **passphrase strength** (+ the Endospore
SE/Weaver throttle), not "there is physically nothing there." Marketing must say the precise version —
*"ciphertext at rest, unrecoverable without your passphrase"* — not the absolute one. Power-off + boot-wipe still
restores nothing-physically-kept.

## What it costs: ephemeral → non-ephemeral + a managed lifecycle

Today's roamed users are **ephemeral** (`MAKE_USER_EPHEMERAL`), where *stop = remove = crypto-shred* — the
simplification that powers amnesiac-on-idle. The resumable model needs:

- **Non-ephemeral** roamed users, so `stopUser` **FBE-locks** instead of deleting.
- A manual **lifecycle**: idle-timeout → `stopUser` (lock); keyguard unlock → `startUser` + `unlockUser`
  (resume); explicit logoff → `removeUser` (crypto-shred).
- A **boot-time wipe** of any leftover roamed-user `/data`, so an *unclean* power-off (battery death) still ends
  up amnesiac — otherwise the encrypted `/data` persists across the reboot.
- Rework of the **L4 idle watcher** (lock instead of `LOGOUT`) and the **reap** path (the two-phase
  switch-then-reap becomes switch-then-lock for idle, switch-then-remove for logoff).

This is a **redesign of the L1–L5 user lifecycle**, not a config change.

## Caveats / decisions

- **"Nothing kept" softens *while idle-locked*.** The encrypted `/data` sits on disk (nothing *readable*; a
  stolen idle-locked phone is FBE + hardware-throttled). The power-off + boot-wipe restores
  *nothing-physically-kept*. So the strong amnesiac claim holds for the **off** state; the idle-locked state is
  "encrypted, like any secure phone."
- **Per-profile: resume vs panic.** A duress / high-security profile keeps today's **wipe-on-idle** (leaves no
  trace); a convenience profile gets **lock-on-idle resume**. Mirrors the existing per-profile `idle=` pref —
  which now means the *lock* threshold, with a separate (longer) optional *wipe-after* bound.
- **Idle picker (resumable model)** *(Chester, 2026-06-24)* — when the threshold means **lock-and-resume**
  (not re-download), the picker offers **15 min / 1 h / 6 h / 12 h / 24 h / Never**, default **12 h**. Longer
  than the current strict-amnesiac picker (2 / 15 / 30 min, 1 h, Never; default **1 h**) because idle no longer
  costs a full re-download on resume — it just FBE-locks in place — so a 12 h default trades a longer encrypted-
  at-rest window for far fewer re-auths, while `Never` + the boot-wipe still bound it. **The current build keeps
  its 1 h default + existing picker unchanged**; these values land only when the resumable lifecycle ships.
- **Forensic trace.** A non-ephemeral idle-locked user leaves an *encrypted* `/data` trace until power-off (vs
  the current no-trace full wipe). Duress profiles avoid it via wipe-on-idle.
- **Credential strength.** Resume is only as strong as the keyguard credential + the hardware throttle: full
  **passphrase** on the FP3's TEE; a short **PIN** is safe only on **Endospore** (Titan M2 / Weaver — ties to
  the SE-throttle, #18).
- **Failed attempts** at the idle-lock keyguard still trip the L4 limit → brute-force → amnesiac wipe.
- **Continuous store-seal still runs** before the lock, so a lock never strands data; the store copy remains the
  off-device backup and the basis for moving the identity to another device.

## Implementation plan (L1–L5 lifecycle redesign)

Phased so each step is independently verifiable on the FP3. **The L3 lesson governs: prove at-rest, don't assume
it** — P1 isn't done until `/data` is shown to be ciphertext after the stop.

- **P1 — non-ephemeral lifecycle + VERIFIED cold-at-rest.** Create roamed users **non-ephemeral**. Lock =
  `stopUser` (no remove). **Verify** `/data/user/N` is ciphertext and the CE key is evicted after the stop (the
  L3 test, applied to user-stop rather than screen-off). Resume = `startUser` + `unlockUser(credential)` →
  decrypt in place. Logoff = `stopUser` + `removeUser` (crypto-shred). Foundation — nothing else ships until
  cold-at-rest is proven on hardware.
- **P2 — boot-wipe (the power-off amnesiac guarantee).** On boot, crypto-shred any leftover roamed-user `/data`,
  so an **unclean** power-off (battery death) still ends amnesiac (a non-ephemeral user's encrypted `/data` would
  otherwise survive the reboot). Verify: pull power mid-session → boot → no roamed `/data`.
- **P3 — L4 watcher: lock, not logout.** Idle ≥ T (12 h default) → **lock** (`stopUser`), not the seal+wipe
  `LOGOUT`. Add an optional longer **wipe-after** bound (crypto-shred a phone left locked for days even without a
  power-off). Failed-attempts still **wipe** (panic). Two-phase reap becomes *switch-then-lock* for idle vs
  *switch-then-remove* for logoff.
- **P4 — resume UX + the 12 h picker.** Keyguard unlock → `startUser` + `unlockUser` + switch back (cold app
  restart, **no re-download**). Make the resumable picker live; the per-profile `idle=` pref now means the
  **lock** threshold (with the separate, longer wipe-after bound).
- **P5 — seal cadence + the optimizers.** The full seal now runs only on **explicit roam/logoff**, the
  **wipe-after** bound, and a **periodic backup** (rarer than today's per-idle seal — a cold lock no longer needs
  one; the data is safe locally, encrypted). The **blob cache (#146, network)** and **content cache (#1, local
  crypto)** make those seals fast — #1 becomes the *roam/backup-seal* optimizer, not a per-logoff necessity.
- **P6 — per-profile + duress.** Default = resumable-cold (lock-on-idle). High-security / duress = **wipe-on-idle**
  (no encrypted trace). Mirrors the existing per-profile `idle=` pref.

## Decisions (resolved)

- **Boot-wipe policy → STRICT** (Chester, 2026-06-30): wipe ALL roamed `/data` on every reboot/shutdown — **no**
  keep-encrypted-across-reboot. A powered-off device holds nothing, not even ciphertext, so the strong amnesiac
  claim carries no asterisk. (The idle-locked *while-on* state still keeps user-bounded ciphertext — that's fine
  and user-controlled; see the thresholds below.)
- **Credential → PASSPHRASE, both editions** (Chester, 2026-06-30): a short PIN/pattern that *roams* can't be made
  safe against the zero-knowledge store without a server oracle (which would hold a per-profile unlock handle = a
  profile list, eroding blind login) or a user-carried token; and a short PIN for *idle-resume* is only safe behind
  Endospore's Titan M2/Weaver throttle, not the FP3's TEE. Since strict boot-wipe makes the PIN's main payoff
  (tap-to-resume after a reboot) moot anyway, **the PIN/pattern work is parked** — passphrase is the credential for
  both cold login and idle-resume. (Tracked: #55 roaming PIN, #18 Endospore SE PIN.)
- **Threshold split → DONE** (DIA-20260626-03): two per-profile thresholds shipped — **lock-after** (`idle=`,
  ~15 min default → cold-lock) and a separate longer **wipe-after** (`wipe=`, ~12 h default → crypto-shred a phone
  left locked for days even without a power-off).

## Open questions

- **Multi-identity:** the boot-wipe + the per-identity FBE keys keep one device's identities isolated, but the
  *count* of idle-locked users is a (small) usage signal until the next boot-wipe.

## Net

This is the **lock model going forward** (decided 2026-06-25) — it dissolves the three-way tension and makes the
idle-default question moot (idle → encrypted + resumable, not a re-download), and at 12 h-cold it's a security
*upgrade* over the 1 h-warm interim, not a softening of "amnesiac." It is a focused redesign of the L1–L5
lifecycle (plan above), scheduled on its own; the strict-amnesiac + 1 h-idle stack shipped 2026-06-24 remains the
correct interim until P1's cold-at-rest is proven on hardware. The seal-speed work (blob cache #146, content
cache #1) lands underneath it as the roam/backup-seal optimizer.
