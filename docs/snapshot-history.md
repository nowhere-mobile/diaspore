# Profile snapshot history (#58) — keep the last-K good heads + fallback recovery

## Problem
The roam data-safety arc so far stops the **device** from destroying a good head:
- #72 — never seal a partial/incomplete restore over the good head (per-ref restore receipts).
- #73 — reap failed/replaced sessions; one live user per profile.
- #86 — a FAILED logoff seal never reaps (keeps the session signed in).

But a head can still go bad from the **store side**: a torn PUT or bit-rot corrupts a chunk. #207
surfaced exactly this on profile `b` — login died deterministically at `fetch chunk 667: cipher: message
authentication failed`, and `b` is **bricked at that head** with no way back. The head lives at a single
mutable pointer (`ref/<profileRef>`) that every push overwrites, so the previous good head is gone.

**#58 = keep the last-K good heads retrievable so a corrupt/bad current head can roll back to an earlier one.**

## What already exists (so this is mostly additive)
- **Head** = a signed `vault` header at `ref/<profileRef>` → `Manifest` (content-hash of the DK-sealed CDC
  chunk-manifest) → chunks `blob/<hash>`. Fields: monotonic `Version`, `LastSeal`, HMAC `Sig` (DK-keyed,
  tamper-evident; canonical = header with `Sig=""`). Uses `omitempty` so new fields don't break old sigs
  (that's how `LastSeal` was added). See `vault` struct + `signVault`/`verifyVault` in `core/agent/main.go`.
- **Retention is lease-driven.** The gateway GC (`nowhere-cloud .../gateway.go` `GC()`) reaps only refs whose
  **lease lapsed** > GraceEpochs ago. The store keeps whatever the device keeps leasing. `profileFootprint`
  (`core/agent/billing.go`) builds the leased ref-set = `blob/<head>`, `ref/<profileRef>`, `blob/<manifest>`,
  and `blob/<each chunk>`.
- **Chunks are content-addressed + deduped** — the footprint `refs` map collapses shared chunks.

## Key insight — this is cheap
Because of CDC dedup, keeping the last-K heads alive costs only the **accumulated delta chunks** across those
K versions, not K× the profile. A mostly-stable profile's last 10 heads ≈ the size of the current head plus a
few small deltas. `K` bounds the worst case (a churny/media profile).

## Design
1. **History in the vault header (client-side, signed, rides for free).**
   Add `History []headSnap` to `vault`, each `{Version uint64, Manifest string, Time int64}`, newest-first,
   capped at `K-1` (the live head is the Kth). `omitempty` so old signatures keep verifying; `Sig` covers it
   (it's part of the canonical header). On each push: prepend the OUTGOING head's `{Version, Manifest, Time}`
   to `History`, truncate to `K-1`.
2. **Lease the last-K manifests' chunks.**
   `profileFootprint` unions the chunks (+ each snapshot's `Manifest` blob) of every `History` entry into the
   leased `refs` map. Dedup collapses overlap → only delta chunks add cost. So the gateway GC keeps the last-K
   heads' data alive. Billable under the existing per-GiB footprint math; surfaced in `GET-USAGE`.
3. **Recovery — roll back to a snapshot.**
   - **Manual:** Profile → Your data → **"Restore a snapshot"** → lists `History` (version + timestamp + size)
     → pick one → confirm → the head rolls back.
   - **Auto on login failure:** when a restore fails at a corrupt/missing chunk (the #207 "corrupt chunk in
     store" verdict), the gate offers *"Couldn't load your latest data — restore your last good snapshot?"* →
     same rollback.
   - **Rollback is a FORWARD write:** it writes a NEW head at `version = currentVersion + 1` whose `Manifest`
     is the CHOSEN older snapshot's manifest (and pushes the older `History` accordingly). This is critical —
     it is not a version regression, so the opt-in rollback anchor (replay guard) and the #72 receipts still
     hold. The next login restores the older-but-good data.

## Retention — REVISED to two-class (2026-07-02, after on-device testing)
The first cut ("last-K seals") churned badly on-device: the ~1/min periodic sync rewrites a few chunks from
**background app activity** (logcat/caches, SharedPreferences, SQLite WAL, "last used" timestamps) even with
no user action — so the manifest changes every seal, the K window filled with the last few *minutes* of
auto-saves, and a listed version could be **evicted before the user confirmed** (the observed
`no retained snapshot` failure). Fix = **two classes**:
- **Manual** saves ("Back up now") are deliberate save-points → keep the newest `NOWHERE_SNAP_MANUAL` (5),
  **PINNED** (an automatic sync can never evict them). Labeled *"Backup · <when>"* (accent) in the list.
- **Automatic** saves (periodic sync) → **time-spaced**: newest `NOWHERE_SNAP_AUTO_RECENT` (2) + one/hour for
  `NOWHERE_SNAP_AUTO_HOURS` (6h) + one/day for `NOWHERE_SNAP_AUTO_DAYS` (5d), capped at `NOWHERE_SNAP_AUTO` (5).
  Labeled *"Auto-save · <when>"* (muted). Logoff seals count as automatic.

Each snapshot carries a `Kind` ("manual"/""); the live head's kind rides the vault's `HeadKind` and is stamped
onto the snapshot when the next seal pushes it into `History`. The daemon sets `NOWHERE_SEAL_KIND=manual` for a
`BACKUP` via `roam.req` line 6 → the worker exports it → the seal tags the head. A stale-version rollback now
returns `NOSNAP` (→ "that version is no longer available, pick another") instead of a misleading connection error.

## Decisions (agreed 2026-07-02)
1. **Retention = two-class** (above): 5 pinned manual + ~5 time-spaced automatic. (Superseded the flat K=5.)
2. **Recovery UX = both.** Ship the manual **"Restore a snapshot"** screen (Profile → Your data) AND the
   gate's auto-offer on a failed restore together (P3 + P4) before landing.
3. **Billing:** the retained delta chunks count toward the profile's footprint/quota (consistent + cheap due to
   dedup) — snapshot retention is NOT free.
4. **`b` is NOT retroactively recoverable by this.** `b`'s prior heads were never indexed, and each was
   overwritten, so their manifest hashes are lost. #58 is **forward-looking** protection. `b` is tracked as a
   separate one-off (forensic manifest recovery from the device's local content cache, or accept the loss).

## Phased build
- **P1 — history:** add `History` to the vault; populate/truncate on push; `Sig` covers it. Byte-identity +
  old/new signature-compat tests.
- **P2 — retention:** `profileFootprint` (and the lease/`GET-USAGE` path) union the last-K manifests' chunks;
  prove the gateway GC keeps them (a leased older head survives a GC sweep).
- **P3 — manual recovery:** agent `rollback-head <name> <pass> <version>` verb (forward write) + a
  **"Restore a snapshot"** screen in Profile → Your data (lists `History`, confirm, roll back).
- **P4 — auto-offer at the gate** on a failed restore (ties into #207's corrupt-chunk verdict).
- **P5 — prove on FP3:** push several versions, point the head at a bad/corrupt snapshot, roll back, confirm
  login restores the older good data.

## Verification
- **Unit:** vault `History` round-trip; a header signed pre-#58 still verifies, and a `History`-bearing header
  verifies; `profileFootprint` unions the last-K chunks (dedup collapses overlap); `rollback-head` writes a
  monotonic-forward head whose manifest == the chosen snapshot.
- **Live gateway (VM):** push v1..v4, confirm the GC keeps v1's unique chunks while they're in `History`, then
  drops them once they age out of the last-K window.
- **On-device (P5):** multi-version push on the FP3, simulate a corrupt current head, manual rollback → clean
  restore of the older good session.

## Risks
- **Churny profiles** (media) inflate the delta window → `K` bounds it; `NOWHERE_SNAPSHOT_K` tunable; pairs
  with the on-demand-media direction (#84).
- **Vault header growth:** K small entries → negligible.
- **Rollback must be a forward write** (version N+1 with an old manifest) or it trips the rollback-anchor
  replay guard — explicit in P3.
- Coordinates with #72 receipts (a rollback is a deliberate head write, exempt like create/promote) and the
  cap-flush lease path (P2 changes the leased set the seal pays for).
