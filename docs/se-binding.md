# Endospore E.3b — Secure-element key binding (StrongBox / Titan M2)

Status: **DESIGN (2026-06-20).** The distinguishing security feature of the Endospore (GrapheneOS/Pixel)
edition over Diaspore (FP3): bind passphrase unlock to the **Titan M2** so the passphrase is **not
brute-forceable offline**. Pairs with [endospore-spec.md](endospore-spec.md) (E.3) and builds on the vault
model in [recovery.md](recovery.md).

## Goal (Option A, chosen 2026-06-20)

The FP3 tops out at: a random **Data Key (DK)** encrypts the data; DK is wrapped in keyslots — a `pass`
slot (KEK = `Argon2id(passphrase, salt=name)`) and a `recovery` slot (KEK from the 12-word mnemonic). The
**weakness**: an attacker who captures the (zero-knowledge) store ciphertext can **brute-force the
passphrase offline** against the `pass` slot — there is no rate-limit, because the KEK must be derivable
from name+pass alone for blind-login to work on any device.

Endospore closes this on the **trusted device** using the Titan M2:
- **Trusted device:** the `pass` slot's KEK is gated by the secure element — unlock requires a hardware
  **Weaver**-throttled passphrase check that releases a **StrongBox**-bound device secret mixed into the
  KEK. Wrong guesses are rate-limited in hardware; the device secret never leaves the SE; so the passphrase
  is **not brute-forceable offline** (you need the specific Titan M2, and it throttles you).
- **Roaming to a new device:** falls back to the **12-word recovery mnemonic** (high entropy → not
  passphrase-guessable). Already built (`recover`/`recoverVault`). The new device can then *enroll* (add its
  own SE-gated `pass` slot) so subsequent logins there are passphrase-only + throttled.

This is the Signal-SVR / iCloud-Keychain-HSM shape: a high-entropy master key (our DK) released by an
HSM/SE after a throttled low-entropy secret (the passphrase), with a high-entropy recovery path.

## Amnesiac vs. persistent SE enrollment — RESOLVED (no carve-out needed)

Initial worry: StrongBox/keystore2 state lives in `/data`, which an amnesiac device wipes on power-off, so
the enrollment would die each boot. **That misreads the model.** Endospore's amnesia comes from the roamed
identities being **`MAKE_USER_EPHEMERAL`** Android users (crypto-shredded on power-off) plus the tmpfs
working set — **NOT a wipe of user 0**. User 0 (the gate / device owner / OS-side state) **persists** across
power-off (proven on HW: the provisioning marker survives reboots). The chooser runs **at user 0**, so the
StrongBox key it creates lives in user 0's keystore and **persists naturally** with the device — no
special carve-out engineering required. (Confirmed by Chester 2026-06-20.)

So the SE key is **device state, like the OS** — not user data. The **"amnesiac for your data" claim is
unchanged**: every roamed identity is still ephemeral; the device merely remembers *that it is a trusted
device* via an SE-gated unlock token that is inert without your passphrase + that physical Titan M2. A
brand-new / un-enrolled device holds nothing and roams via the mnemonic — full amnesia there. (The only
state that does NOT persist is intentional: a factory reset / unlock wipes user 0 too, which re-provisions
from scratch — the device then re-enrolls on first login, exactly as a new device would.)

## Crypto mechanism

Add a keyslot kind to the vault (`docs/recovery.md`): **`se`** (device-bound), living **in the roamed vault**
so the SE device can recover DK from the store. Enrolling a device adds its `se` slot **and DROPS the
pass-only slot** (the `Hardened` flag) — this is what closes the offline gap: a captured head then has only a
`recovery` (mnemonic) slot + per-device `se` slots, **none openable by passphrase alone**. The head ref stays
`profileRef(name,pass)` (findable by the legit user); only (passphrase + that device's `se_secret`) or the
mnemonic opens it. **Per-identity opt-in** (the `Hardened` flag): Endospore identities harden; Diaspore/FP3
identities keep the `pass` slot (portable, no SE). **Tradeoff:** a hardened identity can no longer be opened
by passphrase ALONE on a non-enrolled / non-SE device — those roam via the mnemonic, then re-enroll.
Implemented + unit-tested in `core/agent` (E.3b.2): `kekSE`, `enrollSE`, `resolveKeySE`, the `se` fallback in
`resolveKey`/`pushProfile`, and the `enroll-se` CLI.

- **SE-KEK** = `Argon2id( passphrase , salt = H(name ‖ se_secret) )` — a slow KDF, so it stays
  brute-force-resistant even if `se_secret` ever leaks; `se_secret` is a 32-byte random value sealed in the
  Titan M2 and released **only** after a successful **Weaver** check of the passphrase (hardware throttle:
  exponential backoff / lockout after N failures). The StrongBox-bound key is **non-exportable** and
  **auth-bound** (requires the Weaver/credential auth), so `se_secret` cannot be extracted off-device.
- **Enroll** (first login on a device, after a mnemonic recovery OR an already-unlocked session): generate
  `se_secret` in StrongBox + a Weaver slot for the passphrase; wrap DK under `SE-KEK`; persist the wrapped
  DK + handles in the device-local enrollment store (carved out from the wipe).
- **Unlock** (subsequent boots on the enrolled device): passphrase → Weaver check (throttled) → release
  `se_secret` → derive `SE-KEK` → unwrap DK → roam as usual. No store round-trip needed to *derive* the key
  (the store still holds the data; the `se` unlock just yields DK locally).
- **Roam to a new device:** mnemonic → `recovery` slot → DK → (optional) enroll on the new device.

## Integration (who does what)

StrongBox + Weaver are **Android Keystore / keystore2 APIs (Java/binder)** — the Go agent cannot call them
directly. So:
- **`core/chooser` (Java, platform-signed, device owner):** owns the SE operations — StrongBox key gen
  (`KeyGenParameterSpec.setIsStrongBoxBacked(true)` + `setUserAuthenticationRequired`), the Weaver/credential
  throttle, enroll, and unlock. On a successful unlock it hands the **unwrapped DK** to the daemon.
- **`core/agent` (Go daemon):** gains a way to accept a **caller-supplied DK** for a profile (a new
  `login-with-dk` path on the `diaspore_login` socket) instead of always deriving the pass-KEK itself; the
  rest of restore/seal is unchanged. Also a vault helper to add/read the `se` slot metadata.
- **Wipe carve-out (`core/vendor-common` + the amnesiac init):** the power-off wipe must preserve the SE
  enrollment store + the keystore2 entries for the StrongBox key / Weaver slot, while wiping everything
  else. This is the trickiest piece on GrapheneOS (FBE + the credential system) — its own slice.

## Threat model — what the SE buys / doesn't

- **Buys:** on the enrolled trusted device, the passphrase is **not offline-brute-forceable** (need the
  physical Titan M2 + it throttles); insider-/store-operator-resistant (the store still holds only
  ciphertext, and now the passphrase guard is hardware-throttled); a stolen powered-off device resists
  passphrase guessing (Weaver lockout).
- **Doesn't:** a brand-new device's security rests on the **mnemonic's** entropy (treat the 12 words as the
  real root secret — already true). The carve-out means an enrolled device is **not 100% amnesiac** w.r.t.
  the unlock token (acceptable per the decision above; the token is inert without the passphrase + that SE).
- **Attestation** (proving the device is genuine Endospore) is a *separate* feature (Option C) — out of
  scope here; note that key attestation reveals our custom AVB key (see endospore-spec.md risks).

## Implementation slices

1. **E.3b.1 — this design + carve-out decision. DONE** (carve-out confirmed 2026-06-20; design corrected: `se` slot in the roamed vault + drop the pass slot, not a device-local blob).
2. **E.3b.2 — agent: DONE.** `Hardened` vault flag + per-device `se` keyslot; `kekSE`/`enrollSE`/`resolveKeySE`; `se` fallback in `resolveKey`+`pushProfile`; `enroll-se` CLI; unit-tested (`se_test.go`, full suite green).
3. **E.3b.3 — chooser + plumbing: DONE (pending on-device verify).** Chooser `seSecretHex(name)` derives a
   stable per-identity secret from a **StrongBox HMAC key** (non-exportable, Titan M2; `""` if no SE → FP3
   stays portable). It hardens on CREATE (`ENROLL-SE`) and sends `se_secret` on every `ROAM-IN`. Daemon:
   `ENROLL-SE` verb + `se_secret` threaded through `ROAM-IN` → `.roamsession` → `roam.req` line 5 → `roamd`
   exports `DIASPORE_SE_SECRET` for restore **and** the periodic/logout seal. Agent vet+test green; chooser
   builds in the image (verified on-device at `.5`).
4. ~~**E.3b.4 — wipe carve-out**~~ **— DROPPED (moot).** User 0 persists across the amnesiac power-off (only
   `MAKE_USER_EPHEMERAL` roamed users + tmpfs are wiped), so the user-0 StrongBox key persists naturally —
   no carve-out needed. See the amnesiac section above.
5. **E.3b.5 — on-device: DONE + PROVEN on a Pixel 7a (2026-06-20).** `lynx-cur-userdebug` flashed; created
   `setest` at the gate → confirmed: (1) `feature:android.hardware.strongbox_keystore` present on lynx; (2)
   the chooser derived a 32-byte `se_secret` (`.roamsession` line 4 = 64 hex, no StrongBox errors) and the
   plumbing carried it create→`ENROLL-SE`→`.roamsession`→`roamd`→agent; (3) **the identity is hardened** —
   agent `restore` with the passphrase **alone** returns `empty -> nothing` (no pass slot → offline brute
   force closed), while `restore` with the StrongBox `se_secret` opens it (`(1 chunks) -> …`). Recovery slot
   preserved (mnemonic roam path; unit-tested in `se_test.go`). **The offline-brute-force fix works on real
   hardware.** Then **E.3a relock** as the finalization (signed factory image staged — but it predates the
   E.3b code, so re-sign the E.3b user build first).
   *(Deferred refinement: bind the StrongBox key to a device-credential = passphrase for a hardware throttle
   on on-device guessing — not needed for the offline-brute-force fix, which non-extractability already gives.)*
6. **S1 — on-device guessing throttle (the deferred refinement above). PLANNED 2026-06-22.** Bind the
   StrongBox `se_secret` key with `setUserAuthenticationRequired(true)` to the **lock-screen device
   credential** (see [lock-model.md](lock-model.md) "Implementation plan", L1+L2) so releasing `se_secret`
   requires a fresh credential auth → on-device passphrase/PIN guessing is hardware-throttled (Weaver
   backoff / lockout). This is the *complement* to the already-closed offline path: offline brute-force is
   shut by non-extractability (E.3b.5), S1 shuts on-device brute-force. **Depends on the lock-screen
   credential existing** — that credential is the throttle anchor, which is why S1 sequences after L1+L2.
   Confirm **Weaver is reachable on lynx**; otherwise fall back to the coarser Keymint
   `setUserAuthenticationValidityDuration` (see Open decisions). Tracked as task S1.

## Open decisions
- ~~The carve-out (amnesiac-for-data + persistent-SE-unlock)~~ — **CONFIRMED by Chester 2026-06-20** (per-identity `Hardened` opt-in; FP3 unaffected).
- **Throttle primitive:** Weaver (preferred — true hardware throttle) vs. a Keymint auth-bound StrongBox key
  with `setUserAuthenticationValidityDuration` (simpler but coarser). Confirm Weaver is reachable on lynx.
- **Multi-device:** how many enrolled `se` slots per identity; revocation of a lost enrolled device.
