# Diaspore — Recovery & passphrase reset (key-wrapping / keyslots)

Status: **design** · 2026-06-14 · target: Phase 1 productionization (data-safety)

## Problem

Today an identity's encryption key **and** its store location are both derived directly from the
passphrase:

- `key = Argon2id(passphrase, salt(name))` — encrypts all of the profile's data.
- `ref = sha256("diaspore-ref:" + name + "\0" + passphrase)` — the mutable head's location in the store.

Consequences:

- **No recovery** — forget the passphrase and the data is cryptographically gone. For a real product this
  is the single biggest data-safety gap.
- **No rotation** — changing the passphrase changes the key *and* the ref, so it would require
  re-encrypting and re-uploading the **entire** profile under a new key at a new location.

The **blind-login** property (wrong passphrase / unknown name → an unguessable ref → a uniform blank, so
neither profile existence nor a typo-vs-wrong-profile distinction leaks) must be preserved.

## Model: the passphrase *unlocks* the key, it doesn't *become* the key

Borrow the LUKS / password-manager design.

- A random per-profile **Data Key (DK)** (32 bytes) encrypts the profile's data (chunks + manifest).
- DK is **wrapped** (AES-256-GCM, random nonce) under one or more **key-encryption keys (KEKs)** and
  stored in a small **vault header**:
  - `KEK_pass     = Argon2id(passphrase,        salt = sha256("diaspore-salt:"          + name))` — everyday unlock.
  - `KEK_recovery = Argon2id(recovery_entropy,  salt = sha256("diaspore-recovery-salt:" + name))` — escape hatch.
- The data (chunks, manifest) is sealed under **DK** — *not* the passphrase-derived key — so it never has
  to be re-encrypted when the passphrase changes.

### Vault header

The head blob at the ref becomes the vault. It is **plaintext JSON**: the only secrets are the *wrapped*
DKs, which are useless without a KEK — exactly like a LUKS header, it is safe to read without a key.

```
{
  "v": 1,
  "slots": [
    { "kind": "pass",     "salt": "<b64>", "wrapped": "<b64: nonce|ciphertext|tag>" },
    { "kind": "recovery", "salt": "<b64>", "wrapped": "<b64: nonce|ciphertext|tag>" }
  ],
  "manifest": "<content-hash of the DK-sealed CDC chunk-manifest>"
}
```

`manifest` points at the DK-sealed CDC chunk-manifest blob — the same structure as today's head, just
sealed under DK.

### Refs — blind-login preserved

Two unguessable refs point at the same vault blob:

- `ref_pass     = sha256("diaspore-ref:"          + name + "\0" + passphrase)`   (unchanged from today)
- `ref_recovery = sha256("diaspore-recovery-ref:" + name + "\0" + recovery_entropy)`

Finding the vault still requires the name **and** a secret (passphrase or recovery code), so neither
enumeration nor existence leaks. A wrong passphrase still yields an unguessable `ref_pass` → no vault →
uniform blank, exactly as today.

## Recovery code: a 12-word phrase (BIP39)

128 bits of entropy → 12 BIP39 English words + checksum. Easy to write by hand, the checksum catches
transcription typos, and the format is familiar from crypto wallets. Shown **once** at profile creation
(and once at migration). `recovery_entropy` is the 128-bit entropy those words encode.

## Flows

- **Create** — generate a random DK + 128-bit recovery entropy (→ 12 words, shown once). Wrap DK under
  KEK_pass + KEK_recovery → vault. `putRef(ref_pass, vault)` and `putRef(ref_recovery, vault)`.
- **Login** — `getRef(ref_pass)` → vault → unwrap DK from the `pass` slot → DK decrypts the manifest +
  chunks. (Wrong pass → no ref → blank, as today.)
- **Recover** (forgot passphrase) — user types name + the 12 words → `getRef(ref_recovery)` → vault →
  unwrap DK from the `recovery` slot → prompt for a **new** passphrase → re-wrap DK under the new KEK_pass
  → new vault → `putRef(ref_pass_new, vault)` + `putRef(ref_recovery, vault)`. The old `ref_pass` is
  orphaned and harmless.
- **Rotate** (knows the passphrase) — unwrap DK with the old KEK_pass → re-wrap under the new KEK_pass →
  new vault → update both refs. **No data re-encryption** — DK is unchanged.

## Migration — no re-encryption

Existing profiles are sealed under `old_key = Argon2id(passphrase)`. To upgrade **without** re-encrypting
the whole profile, set **DK = old_key** for migrated profiles. Then:

- The existing chunks + manifest (sealed under `old_key` = DK) still decrypt — nothing is re-uploaded.
- Fresh recovery entropy is generated, DK is wrapped under both KEKs → the vault replaces the bare manifest
  at `ref_pass`, and `ref_recovery` is published.

**Auto-upgrade on next login:** when the resolver finds a *bare* manifest (old format) at `ref_pass`
instead of a vault, it migrates after a successful passphrase unlock and surfaces the new 12-word code to
the chooser to show once. New profiles use a random DK from the start. Detection: a vault parses as JSON
with `"v"` + `"slots"`; the old head is a `cdcMagic`-prefixed sealed blob (or a legacy tar). Try
vault-parse first; fall back to the old path + migrate.

## Implementation slices

1. **Agent crypto core** — DK + `wrap`/`unwrap` (AES-GCM, random nonce); the vault struct + (de)serialize;
   BIP39 mnemonic (add `go-bip39`); `KEK_recovery` + `ref_recovery`. New CLI verbs: `create-vault`,
   `recover`, `rotate`; teach `restore`/`push` to resolve DK through the vault. Keep reading the old format
   (for migration).
2. **Daemon** — `CREATE` returns the 12 words; `ROAM-IN` resolves via the vault and auto-migrates old
   profiles (returning the code once); new `RECOVER` + `ROTATE` socket verbs.
3. **Chooser UI** — show the recovery phrase once on create / migrate (with a "write this down" confirm); a
   "Forgot passphrase?" recover-at-gate flow (name + 12 words → set new pass); a "Change passphrase" screen
   for a logged-in profile.
4. **Test on FP3** — create (capture the code) → login → rotate → log in on the new pass → recover (forget
   the pass, use the 12 words) → log in on the reset pass; verify data is intact throughout and dedup is
   unaffected.

## Notes / decisions

- Recovery code = **12-word BIP39**; existing profiles **auto-migrate** on next login (both decided 2026-06-14).
- The vault header is plaintext **by design** — the keyslots are the encrypted parts; it holds only wrapped
  keys + a content hash, and is reachable only with the name + a secret.
- Losing **both** the passphrase and the recovery code is still unrecoverable, by construction — there is no
  escrow. That *is* the zero-knowledge guarantee; the create screen must say so plainly.
- This also lays the groundwork for **multi-passphrase / per-device keyslots** later (more slots = more
  independent unlock paths, e.g. a duress slot or a per-device key).
