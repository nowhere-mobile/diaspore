# Nowhere — KDF-derived store refs (#80, credential-tiers P2)

Status: **design / implementation plan** · 2026-07-02 · target: Phase 1 hardening. See [credential-tiers.md](credential-tiers.md) Tier 1b.

## Problem

`profileRef(name, pass) = sha256("nowhere-ref:" + name + "\x00" + pass)` ([main.go](../core/agent/main.go)) is
the store location of a profile's head. Because it's a bare SHA-256, it's a **fast existence oracle**: anyone
holding a dump of the store's `ref/…` keys can test passphrase guesses for a target name at billions/sec,
offline. The Argon2id in `deriveKey` protects the *data*, not the *ref*, so a weak passphrase falls to the ref
oracle long before the ciphertext matters.

**Goal:** make the ref Argon2-hard, so a store dump costs the full KDF per guess. Do it with **zero added
login latency** (the Argon2 is already computed for the KEK/DK) and **without breaking existing profiles**
(their heads live at the old sha256 ref).

## Derivation

```
K            = deriveKey(name, pass)                    // Argon2id(pass, salt(name)) — already computed for the KEK/DK
profileRefV2(name,pass)   = hex(HMAC-SHA256(K, "nowhere-ref-v2"))
profileRefLegacy(name,pass) = hex(sha256("nowhere-ref:" + name + "\x00" + pass))   // today's profileRef, kept for fallback
```

- **`deriveKey` must be memoized** (process cache keyed by `name\x00pass` → K) so the many `profileRef`/
  `deriveKey` calls per operation cost **one** Argon2 per `(name,pass)`. Without this, v2 refs would add a
  64 MB Argon2 per call. Memoization makes v2 free (K is reused for the KEK, the DK, *and* the ref). The cache
  is per-process (a CLI invocation, or the daemon); K already lives in RAM during the op, so this doesn't
  widen its exposure meaningfully.
- Sub-refs reuse the SAME uniform helper by passing the sub-name: `#de` / `#media` / `#wallet` heads are just
  `headKey(base, name+"#de", pass)` etc. Their `deriveKey(name#de, pass)` is already computed at restore, so
  memoization keeps them free too.
- **Recovery ref stays SHA-256.** `ref_recovery = sha256("nowhere-recovery-ref:" + name + "\x00" + entropy)`
  is keyed by the 128-bit mnemonic entropy, not a guessable passphrase — the oracle can't brute-force it, so
  migrating it buys nothing. Leaving it out trims the migration surface. (Revisit only if we want uniformity.)

## The two choke-point helpers (contain the migration in ONE tested place)

Almost every site is `getRef/putRef/refExists/delRef(base, profileRef(name,pass))`. Replace `profileRef(...)`
with helpers so the 35 sites become mechanical swaps and the migration logic lives in one place:

```
// headKey resolves the ref the head CURRENTLY lives at: v2 if it exists (migrated or new), else the legacy
// sha256 ref if THAT exists (un-migrated), else v2 (a brand-new profile). Memoized per (name,pass); reads use
// it for getRef/refExists/delRef and the bare-ref uses (lease, footprint key, receipt, rollback).
func headKey(base, name, pass string) string {
    v2 := profileRefV2(name, pass)
    if refExists(base, v2) { return v2 }
    if lg := profileRefLegacy(name, pass); refExists(base, lg) { return lg }
    return v2
}

// putHead writes the head at v2 (migrating), then TOMBSTONES the legacy ref so the sha256 oracle no longer
// serves data and a stale fallback can't resurface the old head. Updates the headKey memo to v2.
func putHead(base, name, pass, val string) {
    putRef(base, profileRefV2(name, pass), val)
    if lg := profileRefLegacy(name, pass); lg != profileRefV2(name, pass) && refExists(base, lg) {
        delRef(base, lg) // tombstone (empty-but-exists); reads try v2 first so it's never consulted post-migration
    }
    // memo: headKey(name,pass) -> v2
}
```

- **Read path** (`getRef(base, profileRef(...))`, ~15 sites) → `getRef(base, headKey(base,name,pass))`.
- **Write path** (`putRef(base, profileRef(...), v)`, ~6 sites: createVault, pushProfile ×2, rotate, recover,
  enrollSE, pushSet, migrate-ref) → `putHead(base, name, pass, v)`.
- **Bare-ref uses** (~14 sites: `leaseFree(profileRef,…)`, `refs["ref/"+profileRef]`, restore-receipt key,
  `checkRollback`, `bumpAnchor`, `delRef` in delete, `walletRef`, rotate's old/new ref compare) →
  `headKey(base,name,pass)` (they need whichever ref is current).

`profileRef` itself is retired from callers (kept only as `profileRefLegacy`).

## Per-path migration nuances

- **createVault** — new profile: `putHead` writes v2 only (no legacy exists). Clean.
- **pushProfile (seal)** — the migrate-on-seal point: `putHead` writes v2 + tombstones legacy. A legacy
  profile's FIRST seal after #80 migrates it. Preserve the #19/#12 head-absent vs tombstone logic via
  `refExists(base, headKey(...))` (headKey already prefers the ref that exists).
- **rotate** — reads head via `headKey(name, oldpass)`, writes via `putHead(name, newpass)`, and tombstones
  BOTH `profileRefV2(name,oldpass)` and `profileRefLegacy(name,oldpass)`.
- **recover** — writes the head to `putHead(name, newpass)`; recovery ref (sha256, unchanged) is re-pointed
  as today.
- **delete** — tombstone BOTH v2 and legacy (a profile isn't dead if either ref still serves it). `refExists`
  after must be false for both → blind (blank) login, no resurrection (#12/#19).
- **wallet / #de / #media** — all go through `headKey(base, name+"#suffix", pass)` / `putHead(..., name+
  "#suffix", ...)`. No special helper; the sub-name flows through.
- **footprint / lease** — `refs["ref/"+headKey(...)] = 0` and `leaseFree(headKey(...), refs)` so the gateway
  leases the ref that's actually in use. Bonus: the tombstoned legacy ref becomes unleased → the gateway GC
  eventually removes the object entirely, killing even the existence-leak.
- **discovery / autoPublish** — route the identity→config mapping through `headKey` (verify it doesn't bake
  the sha256 ref anywhere).
- **migrate-ref** (old name-only → name+pass) — legacy one-shot; point it at v2 (`putHead`).

## Blind login is preserved

Wrong name / wrong pass → neither `refExists(v2)` nor `refExists(legacy)` → `headKey` returns v2 →
`getRef` empty → uniform blank. v2 is exactly as unguessable as legacy, just Argon2-costly to grind.

## Test matrix (all must pass before any flash — the migration is ATOMIC)

1. **Fresh v2** — create → head at v2, legacy absent; login + reseal stay v2.
2. **Legacy fallback** — seed a head at the sha256 legacy ref only; login finds it; first seal migrates
   (v2 written, legacy tombstoned); second login finds v2; legacy ref no longer serves data.
3. **Wrong pass** — neither ref resolves → blank (blind login).
4. **rotate** — oldpass→newpass; head at newpass-v2; oldpass refs tombstoned; oldpass login blank.
5. **recover** — mnemonic→newpass; head at newpass-v2; recovery ref still opens.
6. **delete** — both refs tombstoned; login blank; no resurrection on a subsequent seal.
7. **wallet / #de / #media** — each sub-ref migrates like the head; wallet balance survives.
8. **footprint/lease** — leases the current (post-migration v2) ref.
9. **deriveKey memoization** — N profileRef/deriveKey calls for one (name,pass) = 1 Argon2 (assert via a
   counter hook).
10. **Regression** — existing round-trip / footprint (#85) / receipt (#72) / cap / incremental suites green.

## Phasing

- **P2a** — `profileRefV2`/`profileRefLegacy`, `deriveKey` memo, `headKey` + `putHead`; unit tests for the
  helpers + memo in isolation.
- **P2b** — route all read sites through `headKey`.
- **P2c** — route all write sites through `putHead` (migrate-on-seal + tombstone).
- **P2d** — sub-refs (#de/#media/#wallet), footprint/lease, delete, rotate, recover, discovery, migrate-ref.
- **P2e** — the full migration test matrix + existing suite green (`go vet && go test ./...`).
- **P2f** — prove on FP3: a legacy profile migrates on its first seal, re-login works, GC reaps the
  tombstoned legacy ref.

## Risks

- **Atomicity.** A partial routing (some sites v2, some legacy-only) makes a profile unreachable. Land + test
  ALL sites before flashing; the failure mode is a silent "blank login" that looks like data loss.
- **Tombstone existence-leak residual.** v2 kills the DATA oracle; the legacy *tombstone* still leaks
  "a profile existed at this sha256 ref" to someone who already guessed name+pass — but yields no data. Full
  object removal happens via GC (P2f) once the legacy ref is unleased. Acceptable; note it.
- **Gateway/GC alignment.** Leased ref keys change to v2; since the gateway leases whatever `headKey` emits,
  it stays consistent. The unleased legacy ref is GC'd (the bonus above).
- **Memo lifetime.** K stays in RAM slightly longer; process-scoped, already resident during the op — minor.
