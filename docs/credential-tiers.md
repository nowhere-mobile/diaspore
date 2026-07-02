# Nowhere — Credential tiers: passphrase floor + FIDO2 keyslot

Status: **design** · 2026-07-01 · target: Phase 1 hardening (floor) + Phase 2/3 differentiator (FIDO2)

## Problem

The passphrase is the **sole roaming secret**, and it carries more weight here than in a normal app:

- There is **no server-side login** — the store is world-readable by design (reads are free; the
  amnesiac-wallet chicken-and-egg), so there is no online throttle, lockout, or 2FA to lean on. The data
  is self-protecting ciphertext; the passphrase is the whole wall.
- `ref = sha256("nowhere-ref:" + name + "\0" + passphrase)` is a **fast existence oracle**: anyone holding
  a dump of the store's ref keys can test guesses for a target name at raw SHA-256 speed (billions/s,
  offline). The Argon2id in `deriveKey` protects the *data*, *not* the ref — a dictionary passphrase falls
  to the ref oracle long before anyone touches the ciphertext.
- A **PIN/pattern cannot roam**: anything derived from 4–6 digits is offline-crackable against a
  world-readable store, no matter the KDF. (A gateway-throttled "stretch oracle" could fix the entropy gap
  but costs availability — no offline login — plus a trust surface and a per-profile linkability channel at
  the gateway. Rejected for now; kept in the back pocket.)

Constraints that shape every fix:

- **Blind login is inviolable**: wrong creds → uniform blank. No check may run at *login*, and nothing in
  the store may reveal that a profile exists.
- **The device is amnesiac**: any credential must work on a *fresh* device with nothing durable on it.
- **Throwaways stay zero-friction** (#4): a local-only session protects nothing store-resident, so it gets
  no ceremony.

## The ladder

| Tier | Credential | For | Mechanism |
|------|-----------|-----|-----------|
| 1 | passphrase | everyone (baseline) | strength **floor** at create/promote/rotate + KDF-derived refs |
| 2a | passphrase **+** FIDO2 key | 2FA | DK unwrap needs both (KEK mixes pass and hmac-secret) |
| 2b | FIDO2 key + its PIN | daily-driver high security | key-primary: hmac-secret is the KEK; the key's own PIN throttles it in hardware |

The 12-word recovery mnemonic ([recovery.md](recovery.md)) remains the universal floor under every tier.
Endospore's SE slot ([se-binding.md](se-binding.md)) is orthogonal: it binds an identity to *one device's*
secure element; FIDO2 binds to a *portable* authenticator — which is what actually matches roaming.

## Tier 1a — passphrase strength floor

**Where enforced — only where data becomes durable:**

- `create-vault` (new stored profile), **PROMOTE** (throwaway → store), `rotate`, `recover` (setting the
  new passphrase).
- **Never at login** (blind login), **never on throwaways** (nothing crackable exists; friction kills the
  feature).

**What (NIST 800-63B-aligned — length/entropy, not composition rules):**

- Floor: ≥ 12 characters or a ≥ 4-word passphrase. zxcvbn-style meter in the create/promote UI.
- Blocklist: the profile name itself, top-N common passwords, keyboard walks, all-one-class trivia.
- A **"generate passphrase"** button (diceware-style, from the bip39 wordlist already shipped for the
  mnemonic) as the low-friction path: strong by default, and the mnemonic is the forget-me backstop.
- Enforced in the **agent** (`create-vault`/`rotate`/`recover` refuse below-floor, overridable for tests
  via env), mirrored in the **chooser UI** (meter + inline error) so the refusal is never the first
  feedback.

**The PROMOTE ceremony** (the one subtle case): a throwaway's passphrase was typed casually — possibly a
typo. Promote is the commitment moment, so it must:

1. Require the passphrase **typed** (not silently reuse the session's copy) — proves the user can
   reproduce the credential their data is about to be sealed under. Choosing a *different* one here is
   free: nothing exists in the store under the old `(name, pass)` (that's what made it a throwaway).
2. Apply the floor (step 1's input).
3. **Refuse** if the chosen `(name, pass)` already resolves to an existing head — otherwise promote would
   seal a local session over someone's real profile. Same accepted ceremony-time leak as `create-vault`'s
   "profile already exists"; the #72 restore-receipt guard backstops this at the seal layer too.
4. Show the recovery mnemonic once (promote creates the vault, so this screen exists anyway).

## Tier 1b — kill the ref existence oracle (KDF-derived refs)

The higher-impact half. Derive the ref from the *same* Argon2 output the login already computes:

```
K    = Argon2id(passphrase, salt(name))          // deriveKey — unchanged, already paid at login
ref  = HMAC-SHA256(K, "nowhere-ref-v2")           // replaces sha256(name+pass)
```

- **Zero added login latency** (the Argon2 is already computed for the KEK); a store-dump attacker now
  pays the full 64 MB Argon2 **per guess** instead of one SHA-256. This multiplies the effective strength
  of every passphrase by the KDF cost — worth more than any complexity rule.
- Blind login unchanged: the v2 ref is exactly as unguessable, just costlier to grind.
- **Migration**: on login, try `ref_v2`, fall back to the legacy sha256 ref; on first successful v2-era
  seal, publish the vault at `ref_v2` and tombstone the legacy ref (same pattern as `migrate-ref` /
  `migrateVault`). The recovery ref gets the same treatment (`HMAC(K_recovery, ...)`).

## Tier 2 — FIDO2 as a vault keyslot (CTAP2 `hmac-secret`)

FIDO2 here is **not** authentication (there is nothing to authenticate against) — it is a *key-material*
source. CTAP2's `hmac-secret` extension holds a per-credential HMAC key inside the authenticator; an
assertion with a salt returns `HMAC(key, salt)` — deterministic, high-entropy, never-exported. (Same
primitive systemd-cryptenroll/LUKS use for FIDO2 disk unlock.) That output slots straight into the
existing vault keyslot array as a third `kind`:

```
{ "kind": "fido2", "credId": "<b64>", "salt": "<b64>", "wrapped": "<b64: nonce|ct|tag>" }
```

`credId` + `salt` are public-safe metadata, exactly like a LUKS header.

- **2a (2FA)**: `KEK = HKDF(K_pass || hmac_output)` — neither the passphrase nor the key alone unwraps DK.
- **2b (key-primary)**: `KEK = HKDF(hmac_output)`; the key's **own PIN** (8 tries → hardware lockout)
  gates the assertion. This is the PIN answer: low-entropy secret, throttled in tamper-resistant hardware
  the *user carries*, fully offline, zero extra trust — the roaming analog of Endospore's Weaver throttle.
  With a **discoverable (resident) credential**, the key also carries the profile name (userHandle) and a
  ref-derivation secret, so login = insert/NFC-tap + PIN. No typing.

**Rules:**

- **External security keys only — never platform passkeys.** A phone-resident passkey dies with the
  amnesiac wipe. USB-C and NFC (FP3 has NFC) roaming authenticators only; keys without `hmac-secret`
  (U2F-only) are unsupported.
- **Enrollment requires the escape hatches**: strongly push ≥ 2 keys (the slots array already supports
  multiples), and re-show the recovery mnemonic at enrollment. Lose-the-key must never mean
  lose-the-data.
- **Blind login preserved**: no key / wrong key / wrong key-PIN → blank, indistinguishable. The v2 ref for
  a key-primary profile derives from the hmac output, so the store still reveals nothing.
- **UV policy is fixed per slot and MUST match enroll ↔ login.** `hmac-secret` derives a *different* secret
  for a PIN-verified (UV) assertion than for a touch-only one (CTAP2 `CredRandomWithUV` vs `WithoutUV`). If a
  slot is enrolled with the key's PIN, every login must use the PIN, or the key returns a different output
  and DK won't unwrap (silent "wrong key" → blank). So the `fido2` slot records its UV policy; **key-primary
  (2b) is always PIN-required (UV)**; a 2FA (2a) slot fixes its choice once at enrollment. Note a no-PIN key
  is a valid *second* factor (passphrase is the "know") but a dangerous *sole* factor — theft = full access.

**The real cost is the CTAP2 client.** De-Googled build → no Play-services FIDO API. The gate (user 0)
must speak raw CTAP2 (CBOR over USB-HID and/or NFC). Bounded, in character with the hand-rolled blind-RSA
work, but it is the bulk of the effort — the agent/vault side is a small new slot kind. Transport order:
USB-C first (simplest HID path), NFC second.

## Not this doc: the in-session lock screen (L5)

The session keyguard PIN is a **local-only** credential: Android throttles attempts, and the 15-min
cold-lock + 12-h wipe backstop it. It never derives store keys and never roams, so a short PIN is fine
there. L5 proceeds independently.

## Phasing

- **P1** — floor + generator + blocklist: agent checks in `create-vault`/`rotate`/`recover`; chooser
  meter/generator UI; the PROMOTE ceremony (typed passphrase, floor, exists-check, mnemonic).
- **P2** — KDF-derived refs (`ref_v2`) + fallback lookup + migrate-on-seal, incl. recovery ref.
- **P3** — `fido2` vault slot in the agent (enroll/open), 2FA mode first (pass + key).
- **P4** — CTAP2 client in the gate (USB-C HID, then NFC); key-primary login UX (discoverable creds).

## Open questions

- CTAP2 client: hand-roll (CBOR + HID framing) vs. vendor a minimal library — audit cost vs. build cost.
- Key-primary ref bootstrap: fixed well-known salt for the ref-slot assertion vs. salt carried in the
  discoverable credential's blob (credBlob extension availability varies by key).
- Floor numbers: 12 chars / 4 words is the opening bid — validate against zxcvbn scoring on-device.
- Whether PROMOTE should *offer* keeping the typed-at-gate passphrase when it already meets the floor
  (one fewer step) or always require the full ceremony (uniformity). Leaning: offer it, pre-validated.
