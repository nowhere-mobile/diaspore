# Diaspore — Storage Plan (the roaming store)

Status: design · 2026-06-09 · applies from Phase 2 (roaming) onward; productionized in Phase 4/5

Where does the user's data actually live? Diaspore keeps **nothing of yours on the device** — your
private state roams, encrypted, in a **store**. This doc pins down what that store is.

## Principle

**The store is a thin, swappable backend behind a content-addressed API, and every byte is
client-side encrypted before it leaves the device.** Therefore the backend is *untrusted for
confidentiality* — it only ever holds ciphertext + opaque (hashed) names. That single property is
what makes the storage layer flexible: it can be a commodity object store, your own server, a P2P
network, or **several at once**.

The device does the crypto (Argon2id(name+passphrase) → keys; AES-256-GCM seal/unseal) and the
chunking (content-defined). The store just keeps bytes.

> Creating a profile, the recovery key, and where the store config *itself* lives on an amnesiac
> device are covered in **[enrollment.md](enrollment.md)** (the "Day 0" flow that pairs with this doc).

## What gets stored (object model)

Two kinds of objects, and only two:

1. **Immutable content-addressed blobs** — the bulk. Sealed (AES-256-GCM) chunks of app private
   data, settings, and files, each keyed by the hash of its ciphertext. Dedup and integrity come for
   free: the same blob is identical everywhere, and a fetched blob is verified by re-hashing.
2. **One small mutable "head" ref per profile** — the pointer to that profile's latest **manifest**
   (the manifest in turn lists the blobs of the working set + the rest, by priority). The ref's
   *name* is `sha256("diaspore-ref:" + profile)` — an opaque hex string, **not** the profile name —
   so the store can't enumerate or correlate profiles by name.

This is exactly the API the on-device agent already speaks, and the `store_server.py` stub already
implements:

```
POST /blob            body=ciphertext         -> returns content hash      (immutable, dedup)
GET  /blob/<hash>                              -> ciphertext
GET  /ref/<name>                               -> current head (manifest hash)   (mutable)
PUT  /ref/<name>      body=head                -> set head
```

**Production = keep this API, swap the backend.** The agent (`phase2/agent`) is just a thin HTTP
client (`getBlob/postBlob/getRef/putRef`); pointing it at a different store is a URL change, not a
code change.

## Backend options (all behind the same API)

| Option | What | Trade-off |
|---|---|---|
| **Managed object store** *(recommended default)* | S3-compatible: Cloudflare R2 / Backblaze B2 / AWS S3, or self-hosted **MinIO** | cheap (~pennies/GB·mo), reliable, reachable anywhere; a provider exists, but it sees only ciphertext |
| **User-controlled** *(privacy-max)* | your own MinIO / Nextcloud / VPS / NAS | zero third party, full control; you are responsible for reachability + availability |
| **Decentralized** *(ethos stretch)* | **IPFS** for blobs (the CID *is* the content hash) + a small signed-ref service for the head | matches the "diaspore" ethos, censorship-resistant; but P2P-from-a-phone (NAT, battery) and the mutable head are genuinely hard |

Because a content-addressed blob is verifiable and identical everywhere, the client can **replicate
across several** stores (e.g. R2 + your own box + IPFS) and fetch from whichever answers first — which
directly hardens the availability weakness below. The blob layer is "commodity"; the only piece that
needs thought is the mutable head.

**Recommendation:** default to a **managed/self-hosted S3-compatible object store** (availability +
simplicity), support **user-hosted** for the privacy-maximal user, and treat **IPFS** as the
decentralized stretch once the mutable-head story is solid.

## Design point 1 — the mutable head (the only hard part)

Content-addressing nails immutable blobs; the per-profile "latest manifest" pointer is mutable, so it
needs two protections the blob layer doesn't:

- **Authenticated updates** — the head must be signed by a key derived from the blind-login
  credentials, so **only the profile owner can move it**. A store (or a thief of the ciphertext)
  cannot forge a new head.
- **Rollback / replay protection** — a monotonic version (counter or timestamp) inside the signed
  head, so a malicious or buggy store can't serve an **old** head to roll you back to stale state.

This is the same problem git refs, IPNS, and Tahoe-LAFS mutable files solve. Diaspore already derives
a per-identity key (P2.3); signing the head with it is the natural fit. *(Not yet implemented — the
stub serves the head unauthenticated; this is a Phase 4/5 item.)*

## Design point 2 — availability is critical

Because the device is **amnesiac**, your private state exists *only* in the store at boot. If the
store is unreachable, you boot to a blank profile (the OS is local and still boots; the *user data*
just isn't there). Consequences:

- Prefer a **managed/replicated** store over a single flaky P2P node — this is the main reason the
  default is an object store, not pure IPFS.
- **Multi-store replication** (above) turns availability into "any one of N is up."
- A degraded-but-usable mode: keep an optional **encrypted warm cache** of the working set so a brief
  outage / offline boot still yields last-known-good user data (the device cache is itself wiped on
  power-off to preserve the amnesiac property — it is a cache, not persistence).

## Design point 3 — metadata / traffic analysis

E2E encryption hides *contents*, not *shape*. A store (or a network observer) still sees blob
**sizes**, **counts**, and **access timing**, which can fingerprint which apps a profile has.
Mitigations (hardening, not yet done):

- **Size-bucket / pad** chunks to fixed sizes so blob sizes carry no information.
- Fetch the **whole working set uniformly** (don't lazily reveal access order for sensitive items).
- Optional **cover traffic** / decoy fetches for high-threat profiles.

## Threat model — what the store can and cannot do

| The store **cannot** | The store **can** (mitigations) |
|---|---|
| read your data (client-side AEAD) | see ciphertext blob sizes/timing → *pad + uniform fetch* |
| enumerate/correlate profiles by name (refs are hashed) | count blobs per head → *bucketing* |
| forge a new head *(once head-signing lands)* | withhold data / go down → *replicate across stores* |
| tamper a blob undetected (content hash) | serve a stale head *(until rollback protection lands)* |

Net: **confidentiality + integrity rest on the device and the math, not on trusting the store.** The
store is trusted only for **availability**, and that is addressed by replication.

## Current state → production path

- **Now:** `store_server.py` — a filesystem-backed HTTP store implementing the blob/ref API; used by
  every Phase 0–3 proof (host-local: `127.0.0.1` / the Cuttlefish host `192.168.97.1`).
- **Production:** the same API over (a) an S3 adapter (MinIO/R2/B2/S3), with (b) **head-signing** +
  **rollback protection** added, and (c) optional **multi-store replication** + **size bucketing**.
  None of this touches the on-device agent beyond the store URL + the head-signature check.

## Phase 4 (FP3) — what changes concretely

The Cuttlefish proofs used a host-local store; a real FP3 on real WiFi needs a store **reachable on
the internet**. For P4.5 (flash + validate) we point the device at a real endpoint — simplest is a
**small bucket or VPS you control** (privacy-maximal, ~free at this scale). The agent is unchanged;
only the store URL differs. Head-signing + replication can follow once the loop is validated on
hardware.

## Open items

- [ ] Sign the head with the per-identity key + add a monotonic version (rollback protection).
- [x] **S3 backend in the agent** — done (`minio-go`; `store` arg = `s3`, config via env). **Validated against
      Filebase (Sia-backed):** push 497 ms, working-set restore 269 ms, full 412 ms, E2E confirmed
      (`P4_2_S3_PASS`). Same code path works for sia.storage S3d (when it ships), Cloudflare R2, Backblaze B2, MinIO.
- [ ] **Continuous background delta-sync** — a looping `diaspore_sync` service that pushes a delta every
      N min (+ on meaningful change / screen-off / while charging), **not only on shutdown**. *Required*,
      not optional: `/data` is wiped on power-off so the store is the only durable copy — an unclean
      power-off (battery/crash/yank) otherwise loses the whole session. Shrinks the loss window to the
      push interval.
- [ ] **Chunk-level content-addressing for delta-efficient push** — replace the current whole-item
      re-seal (random GCM nonce → even unchanged data gets a new hash and re-uploads in full) with
      content-defined chunking + **stable chunk hashes** (the casync/restic model proven in P3.3 [0.23%
      delta] + the Phase 0 sim), so each push transfers only changed chunks. Prerequisite for making
      continuous sync affordable. Needs convergent/deterministic addressing (hash plaintext chunks) so
      unchanged chunks dedup.
- [ ] Multi-store replication + read-from-fastest in the agent.
- [ ] Size-bucket/pad chunks; uniform working-set fetch.
- [ ] (stretch) IPFS blob backend + signed-ref/IPNS head for the decentralized story.
- [ ] Encrypted warm cache for degraded/offline boot (still wiped on power-off).
