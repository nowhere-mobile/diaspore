# Cap-gated I/O: the token/wallet bootstrap (design)

Cap-gated I/O (least-knowledge managed-store) is built through P3 (the write path: a managed device
buffers writes and `capFlush` batch-leases + cap-PUTs them, holding **no store credentials**). Three
problems block *selecting* cap mode on a real device. They look separate but share one root — **you must
pay (lease) to write, but the things that let you pay (the wallet, a new identity) must themselves be
written, and the gateway is blind so it can't do per-user accounting.** This note resolves all three with
**two gateway primitives + two device/product moves**, preserving blindness.

## The three problems
1. **`sealWallet` self-reference** — the wallet holds the tokens, but saving the wallet is a write, which
   needs a lease, paid from… the wallet. Circular, and you'd bleed tokens just to persist tokens.
2. **Day-0 funding** — a brand-new identity has zero tokens but must write its initial vault (`createVault`).
3. **Double-charge** — the seal pays per-write (`capFlush`) *and* the period renew pays (`payRent`); without
   care, the same ref is billed twice in one period.

## The resolution — two gateway primitives

### Primitive A — idempotent (delta) lease  *(fixes #3; makes "re-lease the whole footprint" cheap)*
The gateway already records per-ref `ThroughEpoch` in its ledger. Change `/lease` so **owed counts only the
refs not already covered through the target epoch**:

```
owed = ceil( Σ size(ref) for refs where ledger[ref].ThroughEpoch < targetEpoch  /  GiBPerToken )
```

Re-leasing a ref already covered through the target epoch is a **no-op (0 cost)**. The lease response reports
the **spent token nonces** so the device debits exactly those and keeps the rest. Consequences:
- `capFlush` (seal) leases the *new* chunks → they're uncovered → charged.
- `payRent` (period) re-leases the *whole* footprint → the just-sealed chunks are already covered → free;
  only genuinely-lapsing refs cost. **No double-charge**, and "lease everything each period" becomes cheap.
- Tiny pointers (head, wallet) in the footprint round into the per-GiB math → **~free keep-alive**.

### Primitive B — free small-metadata write caps  *(fixes #1 and #2)*
The gateway issues a **write cap for an object below a small size threshold (e.g. ≤ 16 KiB) without requiring
a live lease**, rate-limited per connection. Reads are already free; this makes *small* writes free too. So:
- the **wallet** (a few hundred bytes) and the **initial vault** (`createVault`: empty-tar + vault header +
  two pointers, all tiny) can be **written without paying** — breaking both the wallet self-reference and
  Day-0.
- **Persistence is still the lease's job, not the write's.** A free-written small object that no lease covers
  is GC'd after grace — so free small writes can't become unbounded free storage. The wallet + vault head
  *are* in the identity's leased footprint (at size ~0, free under Primitive A), so they're kept alive; random
  free-written garbage is reaped.

**Blindness + abuse:** the gateway never learns whose object it is. A blob key is `blob/<hash>` —
**content-addressed and self-verifying**, so you can only write the one key that matches your bytes (can't
poison an arbitrary key). A pointer key `ref/<name>` is **unguessable** (derived from name+passphrase), so
only someone who already knows the identity can overwrite it (their own state). Size cap + rate limit + GC
bound the rest. Net: free small writes are safe and leak nothing.

## The two device/product moves
- **Wallet rides the seal flush (device).** In the common case the wallet changes *because* a seal spent
  tokens. So `capFlush`, after it takes the data tokens, **re-seals the post-spend wallet and writes it in the
  same flush** (added to the same lease set; tiny → free). The roamed wallet then reflects the *final* balance
  — no off-by-one. Standalone wallet writes (a top-up with no data seal, or logout) fall back to Primitive B's
  free small-write cap.
- **Storefront seeds the subscription wallet (product).** A new identity is *created* for free (Primitive B),
  but storing real data needs tokens. Signup / subscription (the linkable storefront, today the dev faucet)
  **mints the initial token batch and seeds the device wallet** before the first data push. UX: sign up →
  establish identity free → tokens arrive with the subscription → store data.

## How each problem is resolved
| Problem | Resolved by |
|---|---|
| #1 wallet self-reference | wallet rides the seal flush (post-spend, free via A's rounding); standalone writes use B |
| #2 Day-0 funding | createVault is small → free via B; real data needs the storefront-seeded wallet |
| #3 double-charge | A — gateway charges only the uncovered delta; payRent re-lease of covered refs is free |

## Implementation sketch & sequencing
- **`nowhere-cloud` (gateway):**
  - A: `handleLease` computes `owed` from the uncovered-delta (consult the ledger per ref); response lists
    spent nonces. Update `internal/ledger` redeem to bill the delta.
  - B: `handleCap` / `/cap?op=write` grants a cap for objects `≤ smallWriteMax` without a live lease,
    rate-limited; GC unchanged (it already reaps unleased refs).
- **`core/agent` (device):**
  - `leaseRefs` debits only the spent nonces the gateway reports (not the optimistic `owed`).
  - `capFlush` folds the post-spend wallet re-seal into the same flush; standalone wallet/vault writes go via
    the free small-write cap (no lease).
  - bracket `sealWallet` (now safe) + the remaining write ops (`rotate`/`recover`/`migrateVault`/`delRef`).
- **Order:** A (idempotent-lease) first — self-contained, unblocks the double-charge and the tiny-pointer
  keep-alive. Then B (free small writes) — unblocks the wallet + Day-0. Then the device folds + brackets.
  Then P4 (select cap mode) and P5 (FP3 creds-free proof + GC reaps an unleased ref).

## Note on blindness vs subscription
A subscription is a recurring *linkable* payment (your card, your tier) that mints a recurring **blind-signed**
token allotment. The provider knows you subscribe and your tier; it cannot link that to which leases/objects
are yours (the gateway sees only valid anonymous tokens + ciphertext). The bootstrap above never weakens
this — A and B operate on refs/sizes the gateway already sees, and neither reveals identity.
