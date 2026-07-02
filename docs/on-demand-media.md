# On-demand media (#84): decouple store quota from device capacity

## Problem

Managed-mode login is **latency-bound, not bandwidth-bound**. A cap-mode restore fetches every chunk with a
presign round-trip to the gateway + an R2 GET; on a ~350 ms RTT link that is ~4 MB/s regardless of the local
pipe (measured: 260 Mbps WiFi, still ~4 MB/s; RTT 187–609 ms). Profile `c` is a worked example: **891 MB of it
is OrganicMaps `.mwm` offline maps** under `/data/user/N/app.organicmaps/files/`. Those maps are *CE app data*,
restored in the exact phase that **gates login** → a ~2.5 GB, ~10-minute login for a profile whose *essential*
working set (launcher, settings, small app state) is tens of MB.

Two goals, one mechanism:
1. **Fast login** — gate only on the essential set; never block login on bulk media.
2. **Decouple store quota from device capacity** — a 250 GB store profile must be usable on a 64 GB phone; the
   device holds a *cache* of media, not the whole thing (ties into the #82 storage warning).

## Approach: deferred media + on-access FUSE cache (B) + eviction (C)

Large, non-essential-at-login files are **classified as deferred media** and split out of the login-gating
manifest. They are served through a **FUSE overlay** (`nowhere_mediad`) mounted over the app's media dir: reads
of a not-yet-cached file lazily fetch its chunks from the store (reusing the agent's `getChunk`/cap path),
decrypt, cache locally, and serve. Under storage pressure the cache is **evicted LRU** (re-fetchable), which is
what decouples store size from device size.

The A/"background trickle" variant (restore deferred files late, no FUSE) is the fallback if B ever regresses;
this doc specifies B because the Phase-0 spike proved it viable (below).

## What the Phase-0 spike PROVED (2026-07-02, FP3, enforcing) — see memory `fuse-media-spike-84`

- Kernel FUSE works; app `/data` is a **slave** of the global mount namespace, so a mount made globally
  **propagates into app namespaces** (mount-over-app-data is viable).
- A loopback FUSE **mounts + serves reads at 345 MB/s** after a small `go-fuse` patch: the Android kernel sends
  `FUSE_CANONICAL_PATH` (opcode **2016**), which stock go-fuse *drops* (never replies) → the read wedges forever.
  Fix = reply `-ENOSYS`, serialized (`req.serializeHeader(0)` before `ms.write`). Loopback throughput is a
  non-issue; the store fetch is the only real latency (same as A).
- **SELinux is solved cleanly** — WITHOUT touching `app_data_file`. Serve files with our OWN type
  `nowhere_media_file` + `mlstrustedobject` (so any per-app MLS category `s0:cN` can read the `s0` file, the same
  MLS-bridge the login socket uses), mount `context=nowhere_media_file`, and `allow appdomain
  nowhere_media_file:{dir,file} r_*_perms`. The policy **compiles with no neverallow violation** (`m
  selinux_policy` clean). This sidesteps the `app_data_file:filesystem associate` wall entirely.
- Remaining confirmation (rides the next real image build): an actual app reading a store-served file *through*
  the mount under enforcing. Every layer it depends on already checks out.

## Components

### 1. Classification — what defers
A policy decides deferred vs essential. First cut: **size ≥ `NOWHERE_MEDIA_MIN` (~8 MB)** in known media
subtrees — app `files/` (maps/model blobs), `Download/`, `DCIM/`, large caches — with an explicit **deny-list**
so nothing login-critical (launcher layout, keystores, account DBs) is ever deferred. Classification runs at
**seal time** (the agent already walks the tree) and is recorded in the manifest, so restore/login know the
split without re-deciding.

### 2. Two-tier manifest
Extend the CDC manifest with a `Tier` per file/chunk: `essential` | `deferred`. The existing phases (CE / DE /
media) stay; `deferred` is orthogonal — a large CE app file can be `deferred`. Old manifests (no `Tier`) read as
all-`essential` (back-compat, like the vault `omitempty` fields).

### 3. Login gates on essential only
`roamInStreaming` / roamd restore the `essential` tier synchronously (what gates the login verdict). Deferred
files are **not fetched at login**; instead the daemon mounts the FUSE media overlay over their dirs so they
appear present (stubs backed by the store). Login → seconds, independent of RTT/bandwidth.

### 4. `nowhere_mediad` — the FUSE media daemon
A new su:s0→dedicated-domain daemon (Go, reusing the agent's chunk/crypto/cap code; go-fuse vendored + the
CANONICAL_PATH patch). Per roamed session it mounts `context=nowhere_media_file` over each deferred-media dir.
On `open`/`read` of an uncached file it fetches the file's chunks (manifest-driven), decrypts, writes them to
the **local media cache** (CE storage, per-user), and serves. `getattr` reports the app's uid + the real size so
DAC + app expectations hold. Read-ahead / fetch-whole-file-on-first-open avoids per-`read()` round-trips (a map
DB does random reads; serving them one network RTT at a time would ANR — fetch the file on first open).

### 5. Sepolicy (validated shape)
```
type nowhere_media_file, file_type;
typeattribute nowhere_media_file mlstrustedobject;
allow nowhere_media_file self:filesystem associate;
allow appdomain nowhere_media_file:dir r_dir_perms;
allow appdomain nowhere_media_file:file r_file_perms;
# + the daemon's own domain: mounton / filesystem { mount relabelfrom relabelto } on nowhere_media_file
```
Ships in `core/vendor-common/sepolicy/`. A dedicated `nowhere_mediad` domain replaces the spike's su:s0.

### 6. Cache + eviction (C)
The local media cache is a CE-per-user dir of fetched files (or chunk blobs). A background reaper evicts **LRU
deferred-media** when `/data` usage crosses the #82 threshold (evicted = local delete; still in the store →
re-fetched on next read via the FUSE path). Essential data and any unsealed local change are never evicted. This
is the "250 GB store on a 64 GB phone" property.

### 7. Seal safety (#72 interplay)
The seal's file-walk MUST treat a **deferred, not-yet-cached** (or evicted) file as **present-in-the-store, not
deleted** — otherwise the next seal drops it. Concretely: the seal reads the manifest's deferred set and carries
those entries forward unchanged unless the app actually wrote the file (detected via the FUSE write path / a
dirty flag). The #72 per-phase restore receipt already refuses sealing an incomplete phase; deferred media must
extend that so "not fetched" ≠ "lost."

### 8. Lifecycle
Mount at login (after the essential restore, before handing off the session); unmount at logoff-reap and at
cold-lock (before the CE key is evicted); survive resumable-cold (remount on resume). Never leave a stale mount
(the reap watcher already tears sessions down; add mediad unmount to that path). **Never** blanket-abort
`/sys/fs/fuse/connections` (it kills Android's own emulated-storage FUSE — cost a live session in the spike).

## Phasing

- **P0 — viability spike: DONE** (mount/read + go-fuse patch + sepolicy compiles + APK). Final app-read
  confirmation rides the next image build.
- **P1** — classification + two-tier manifest in the agent (seal-side); back-compat for old manifests. Unit
  tests. No behavior change at login yet.
- **P2** — login restores `essential` only; deferred files left to the media path. Prove login-time drops to
  seconds on `c` (essential set only).
- **P3** — `nowhere_mediad` FUSE daemon + `nowhere_media_file` sepolicy (dedicated domain) + mount lifecycle in
  roamd/reap. On-access fetch + local cache. Prove OrganicMaps opens a map that streams from the store.
- **P4** — seal safety for deferred/evicted files (#72 extension). Prove a logoff after using maps doesn't drop
  the un-opened ones.
- **P5** — eviction under storage pressure (#82 tie-in). Prove a store-larger-than-device profile is usable and
  media re-fetches after eviction.
- **P6** — prove the full loop on FP3 with `c`: seconds-login, maps stream on demand, evict + re-fetch, seal-safe.

## Risks / open

- **First-`open` latency spike** — fetching a whole map file on first open still pays the store round-trips;
  acceptable (one-time, backgroundable with a progress affordance) but must not ANR the app (fetch off the FUSE
  read thread; return EAGAIN/block with a bounded deadline).
- **Offline reads** — an uncached deferred file read while offline must fail gracefully (EIO the app can handle,
  or a "not downloaded" state) rather than hang.
- **Write-back** — if an app *writes* into a deferred dir, the FUSE must persist it to the backing + mark it
  dirty so the seal captures it. Read-mostly media (maps) is the common case; handle writes correctly anyway.
- **Per-app mounts** — `context=` is one label per mount; multiple apps ⇒ per-app-dir mounts (fine, more mounts).
- **Endospore parity** — GrapheneOS sepolicy differs; the `nowhere_media_file` shape must be re-validated on lynx
  (E.5x).
