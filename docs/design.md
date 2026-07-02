# Diaspore — System Design

Status: draft · 2026-06-08 · concept/design phase

## 1. Summary

Diaspore turns a phone into a **stateless terminal for a roaming identity**. The OS is
an immutable, content-addressed image pulled from a distributed store at boot; all
runtime writes land in RAM; your personal state (`/data`) is continuously encrypted and
replicated to the store and **wiped from the device at shutdown**. Boot anywhere → your
phone re-materializes. Power off → the device is a blank slate.

**Goals**
- Boot a verified, immutable OS image from a distributed/content-addressed cache.
- Zero sensitive state at rest on the device (amnesiac by construction).
- Optional **roaming** personal state that lives in the store, not the device.
- Boot must work even when the network is degraded (cache, not hard dependency).
- The store is **untrusted** — integrity and confidentiality enforced by the client.

**Non-goals**
- WiFi/networking *in the bootloader* (moved to stage-1 initramfs).
- Uploading the whole OS image on shutdown (push *state deltas*, not the OS).
- Play Integrity / SafetyNet / Widevine L1 compatibility (lost to unlocked bootloader — accepted).
- Cellular as the primary bootstrap transport (WiFi/USB-eth first).

## 1A. Model refinement (2026-06-09) — supersedes netboot-centric framing

After proving init interposition + amnesiac `/data` (and hitting netboot fragility), the model
was refined: **the OS does not need to be netbooted/ephemeral — only user data must never
persist locally.**

- **OS:** stored locally, immutable, dm-verity-protected, content-addressed, updated via
  content-addressed **OTA** (not netbooted each boot). Public → leaks nothing at rest.
- **App code:** public APKs, **re-downloaded per device** from the distributor (Aurora/F-Droid),
  cached locally — not roamed as a secret.
- **Roaming private state (the secret):** app list + app private data + accounts + settings +
  files. Encrypted; restored on boot; replicated at runtime; **wiped on power-off**.
- **Regenerable** (dexopt, caches): regenerate or non-secret local cache.

This drops the two hardest parts — **netbooting the OS** and **WiFi-in-the-initramfs** — because
the only thing needing the network is the user-state restore, which happens late (`/data` is
`latemount`) in Android userspace where WiFi already works. Netboot is demoted to an optional
stretch (zero-pre-provision / kiosk).

Authoritative current docs: **[roadmap.md](roadmap.md)** (updated plan) and
**[app-model.md](app-model.md)** (apps in the roaming world). Sections below that assume
netbooting the OS are superseded by this refinement; the immutable-OS/dm-verity/content-address
and roaming-state/encryption machinery all still apply.

## 2. Design tenets

1. **Immutable OS, mutable identity.** Two separate lifecycles: the OS is shared,
   read-only, pull-only; identity/state is private, writable, push-only. Never conflate.
2. **Content-addressing everywhere.** Hash = name = integrity proof. Ties into dm-verity/AVB.
3. **The network is a cache, not a disk.** Always retain a verified last-known-good locally.
4. **Wipe by construction, not by erase.** Writable layers are RAM-backed; power-off *is* the wipe.
5. **Encrypt before it leaves the device.** The store sees only opaque ciphertext.
6. **Continuous, not shutdown-flush.** State replicates during runtime; shutdown flushes a small tail.

## 3. Architecture overview

```
        +------------------------ SUPPLY SIDE (trusted, you control) --------------+
        |  reproducible build -> OS image (erofs + dm-verity) -> chunker (casync) ->|
        |                         per device/channel              signed manifest   |
        +---------------+------------------------------------------------+----------+
                publish chunks                                    sign channel ref
                        v                                                v
        +------------------------ DISTRIBUTED STORE (untrusted) -------------------+
        |  content-addressed chunk store (S3+CDN / IPFS)   +   ref / manifest svc  |
        |  encrypted roaming-state repo per identity (opaque AEAD chunks + dedup)  |
        +---------------^----------------------------------------+-----------------+
          pull OS chunks | verified by hash                      | push/pull encrypted
                         |                                        | /data deltas
        +----------------+----------------------------------------+----------------+
        |                      DEVICE  (holds nothing sensitive at rest)           |
        |  bootloader(custom AVB key) -> stage-1 initramfs:                        |
        |       WiFi up -> fetch+verify image -> assemble overlay -> Android init  |
        |  overlay root = [ro OS image] (+) [tmpfs scratch] (+) [/data]            |
        |  LineageOS userspace                                                     |
        |  replication agent --(continuous, client-encrypted)--> store            |
        |  shutdown hook: freeze -> snapshot -> flush tail -> WIPE                 |
        +--------------------------------------------------------------------------+
```

Five planes: **build/publish**, **store**, **boot**, **storage/overlay**,
**roaming-state + keys**.

## 4. Components

### A. Build & publish pipeline (supply side)
- **Input:** LineageOS device tree → a read-only **erofs** system image, with a
  **dm-verity** hash tree and signed root hash.
- **Chunking:** run the image through **casync/desync** (content-defined chunking) → a
  chunk store + an index (`.caidx`). Publishing v(N+1) uploads only changed chunks.
- **Manifest:** small signed JSON per channel/version: image index hash, dm-verity root,
  `boot.img` hash, vendor-module set, generation number, min-rollback counter.
- **Channels/refs:** `stable` / `beta` / `dev`, each a signed pointer to the current
  manifest (OSTree-style ref). Boot resolves channel → manifest → chunks.
- **Reproducibility:** reproducible builds so the same source → same hashes (auditable).

### B. Distributed store (untrusted)
Two logical repos, both content-addressed:
- **OS repo** (public, immutable, shared): chunk store + signed manifests. Backed by
  **S3+CDN** (managed) or **IPFS** (P2P). Integrity from signatures + hashes, not the host.
- **Identity repo** (private, per-user): encrypted `/data` snapshots as AEAD chunks,
  restic-style — content-defined chunking on *plaintext* (dedup within your own history),
  each chunk encrypted client-side. Store sees only ciphertext + an access-controlled
  namespace keyed by your identity public key.

The device keeps an **on-flash persistent chunk cache for the OS repo only** (public /
immutable → safe to keep, makes reboots fast). The identity repo is **never** cached in
cleartext on the device.

### C. Boot chain (the stage-1 netboot)
Chain of trust, even with a custom-key bootloader:

```
HW root of trust -> bootloader (your custom AVB key, "yellow" state)
   -> boot.img (kernel + stage-1 initramfs)        [AVB-verified against your key]
       -> baked-in manifest trust anchor (pubkey)
           -> signed channel manifest               [signature-verified]
               -> OS image chunks                   [content-hash-verified]
                   -> dm-verity at runtime          [per-block-verified on read]
```

Stage-1 `/init`: mount pseudo-fs → load WiFi modules+firmware (bundled in initramfs) →
associate + DHCP → resolve channel→manifest (verify sig) → pull+verify OS chunks (local
cache first) → set up dm-verity over erofs → build overlay → hand off to Android init via
a **generated fstab** pointing `system`/`vendor`/`userdata` at the dm devices. Networking
lives here, **not** in the bootloader — the load-bearing decision. (Full flow:
[`boot-flow.md`](boot-flow.md).)

### D. Storage / overlay model
Three layers composed with **overlayfs**:

| Layer | Backing | Lifetime | Contents |
|---|---|---|---|
| **OS** (lower, ro) | erofs + dm-verity, from cache | immutable | system/vendor/product |
| **Scratch** (upper) | **tmpfs (RAM)** | per-boot, auto-wiped | logs, caches, `/tmp` |
| **/data** | tmpfs *or* restored snapshot | amnesiac *or* roaming | apps, accounts, settings |

- **Amnesiac mode:** `/data` is tmpfs → factory-fresh every boot (Tails-like).
- **Roaming mode:** `/data` restored from the identity repo, continuously replicated,
  wiped at shutdown. The device is a window onto network-resident state.

### E. Roaming-state engine
1. **Restore (boot):** fetch latest encrypted snapshot → decrypt → materialize `/data`.
   Lazy/working-set restore so boot isn't gated on a multi-GB download.
2. **Replicate (runtime):** daemon watches `/data`, periodically + on quiescence snapshots →
   content-defined chunking → encrypt new chunks → push. Continuous → small un-replicated tail.
3. **Snapshot consistency:** quiesce writers / fs-freeze or dm-snapshot → crash-consistent
   generations, not torn reads.
4. **Flush + wipe (shutdown):** stop apps → final snapshot → push tail → **verify retrievable** →
   then wipe (tmpfs freed by power-off; `blkdiscard` flash scratch).
5. **Generations:** monotonic, immutable; enables restore + anti-rollback.

Single-writer assumption (one active device per identity) avoids distributed-write
conflicts; concurrent multi-device is later work (needs a lease in the ref service).

### F. Identity & key management
Keys must be re-derivable from something the user carries, not hardware-bound state
(because the device wipes):

```
user passphrase --Argon2id--> Root Identity Key (RIK)   [re-derived each boot, never stored]
   RIK -unwraps-> Master Key (MK)        [stored only as an MK-wrapped blob in the store]
      MK -wraps-> per-generation Data Encryption Keys (DEK)   [rotation w/o re-encrypt]
         DEK -AEAD-> /data chunks
   identity keypair (from RIK) -signs-> writes to the store namespace (authZ + integrity)
```

- Optional second factor: hardware token (FIDO2/PIV) or remote KMS holding part of the secret.
- **Recovery:** passphrase loss = data loss by design; offer a user-held recovery key.
- **Anti-rollback:** monotonic generation counters + signed "head"; pin last-seen generation
  in the (kept, integrity-checked) OS chunk-cache area; reject manifests/states below it.
  Fully robust rollback protection wants a durable monotonic counter, which fights the wipe
  (open issue §10).

### G. Security model
- **Verified boot** with your own AVB key → boot.img/initramfs integrity enforced despite
  custom bootloader.
- **dm-verity** → runtime per-block integrity of the OS image; tamper-evident from an
  untrusted cache.
- **Client-side AEAD** → store confidentiality; host learns only sizes/timing.
- **Physical seizure resistance:** powered-off device has no plaintext `/data` and no
  long-lived keys (passphrase-derived) → strong amnesiac property.
- **Accepted losses:** unlocked/custom-key bootloader ⇒ Play Integrity/SafetyNet fail,
  Widevine L1→L3; banking/DRM apps break.
- **Network adversary:** TLS + signed manifests defeat MITM on the OS path; AEAD + signed
  writes defeat tampering on the identity path.

### H. Profiles & the chooser stage

Multiple roaming identities ("profiles") can share one device. A **profile** = an identity
(its own passphrase-derived keys, its own `head:<identity>` roaming state, optional
per-profile policy). Selection happens in a **chooser**: a shared, immutable, amnesiac boot
stage with no user data, run after the network is up but *before* any `/data` is fetched —
so you choose *before downloading user data*.

- **Access model: blind login** (chosen). No profile list is shown; the user types a profile
  name + passphrase. Strongest privacy: no enumeration, works on a blank device, and enables
  **hidden / duress profiles** (a profile exists only if you type its exact name+passphrase →
  plausible deniability under coercion). Optional later convenience: a device/owner-keyed
  encrypted profile-list object in the store, decrypted after a device unlock.
- **Isolation falls out of the existing model:** each profile's state is encrypted under its
  own keys and namespaced by `head:<identity>`; the amnesiac wipe means switching profiles
  (= reboot → choose another) leaves zero cross-profile residue.
- **Per-profile policy:** each profile may independently be amnesiac (tmpfs) or roaming, and
  may pin a different OS channel. The immutable OS is shared, so it can be fetched/cached
  regardless of choice — only the per-profile `/data` waits on selection.
- **Cost:** a UI before Android exists — a minimal framebuffer/touch picker + passphrase
  entry in the chooser stage (cf. postmarketOS `osk-sdl`), or a stripped "chooser Android"
  that kexecs into the chosen profile.
- **Phase:** Phase 2 (builds on roaming state + identity + keys). Phase 1 reserves the
  chooser hook but ships single-profile amnesiac.

## 5. Key flows

- **Cold boot (network OK):** bootloader → initramfs → WiFi → resolve channel →
  verify+assemble OS (cache hits skip downloads) → restore `/data` working set → Android up.
- **Cold boot (network down):** no connectivity → boot **last-known-good** OS from local
  cache (verity-checked) → `/data` from last local-allowed state or amnesiac → degraded but usable.
- **Runtime:** replication agent streams encrypted `/data` deltas; OS layer never changes.
- **Graceful shutdown:** freeze → snapshot → flush tail → verify retrievable → wipe → power off.
- **Power loss / crash:** un-replicated tail (seconds) lost; next boot restores last good
  generation. No corruption — generations are immutable + atomically committed.
- **OS update:** publish new image+manifest → bump channel ref → next boot pulls only
  changed chunks. Instant rollback = repoint the ref.
- **First enrollment:** user sets passphrase → derive RIK/MK/identity keypair → seed empty
  generation-0 `/data` → push.

## 6. Data model (sketch)

- **Manifest** `{ channel, version, image_caidx, verity_root, boot_img_hash, vendor_set, generation, min_rollback, sig }`
- **Channel ref** `{ channel -> manifest_hash, signed }`
- **State generation** `{ id (monotonic), parent, chunk_list, dek_id, created_at, signed_by_identity }`
- **Key bundle** (in store, MK-wrapped) `{ mk_wrapped_by_rik, dek_table, recovery_blob }`

## 7. Failure modes & mitigations

| Failure | Mitigation |
|---|---|
| Power loss mid-replication | Continuous replication → tiny tail; atomic generation commit; restore last good |
| Network down at boot | Last-known-good local boot from verified OS cache |
| Store unavailable for writes | Buffer deltas in RAM/scratch, retry; block clean shutdown until flushed or user overrides |
| Passphrase/key loss | User-held recovery key; otherwise data loss by design |
| Chunk corruption / bad host | Content-hash + dm-verity reject; re-fetch from another replica/peer |
| tmpfs (RAM) exhaustion | Quota the upper layer; optionally spill *encrypted* to flash scratch; OOM policy |
| Rollback attack (old signed state) | Monotonic generations + pinned min-rollback + signed head |
| Clock skew breaks TLS/cert | Ship a roughly-trusted time anchor in initramfs; NTP after net up |
| SELinux denials on overlay | Bring-up permissive → label policy for overlay/tmpfs → enforce |

## 8. Technology choices (initial)

| Concern | Pick | Why |
|---|---|---|
| RO image fs | **erofs** | Read-only Android-native, compact, fast random read |
| Runtime integrity | **dm-verity + AVB (custom key)** | Per-block verification; reuses Android's chain |
| Image distribution | **casync/desync** | Content-defined chunking, CDN-friendly deltas (vs OSTree's many small files) |
| P2P option | **IPFS** | If "distributed" must mean peer-to-peer, not just CDN |
| Boot transport | **WiFi in initramfs**; USB-eth+**NBD** for dev | pmOS-proven for tethered; WiFi is the untether step |
| State backup engine | **restic-style** (CDC + per-chunk AEAD + dedup) | Encrypted, deduped, incremental — exactly the roaming model |
| Encryption | **libsodium / age**; **Argon2id** KDF | Modern AEAD + memory-hard passphrase derivation |
| Userspace | **LineageOS (GKI device)** | GKI separates generic kernel from vendor → more portable boot image |

## 9. Phased roadmap

- **Phase 0 — prove the loop (emulator).** Cuttlefish/AVD: NBD-root + tmpfs overlay +
  scripted encrypted delta push on shutdown. *Exit: boot from network image, ephemeral
  writes, state survives reboots via the store.*
- **Phase 1 — real device, amnesiac.** One GKI phone: custom `boot.img`, stage-1 WiFi
  netboot, erofs+verity OS from a content-addressed cache, `/data`=tmpfs. *Exit:
  untethered verified netboot, factory-fresh every boot.*
- **Phase 2 — roaming state.** Identity repo: key derivation, continuous encrypted
  replication, restore-on-boot. *Exit: state roams; reboot restores your device.*
- **Phase 3 — resilience.** Last-known-good local boot, OS update channels, anti-rollback,
  lazy/working-set restore. *Exit: degrades gracefully offline; instant OS rollback.*
- **Phase 4 — hardening.** SELinux enforcing, key UX (passphrase + optional FIDO2),
  multi-device lease, recovery flows.

## 10. Open questions / risks

1. **SELinux on overlay** — labeling merged/tmpfs layers under enforcing policy is the
   fiddliest integration seam.
2. **Anti-rollback vs. wipe** — durable monotonic counter wants persistent secure storage,
   which the wipe removes; need a scheme (pinned in OS cache? remote head?).
3. **Apps that assume durable `/data`** — keystore-bound app data sees a "new device" each
   amnesiac boot; roaming mode fixes most, but attestation-bound apps won't.
4. **Restore latency** — full `/data` could be large; lazy/working-set restore needed.
5. **Shutdown time budget** — keep the flush tail tiny; users yank power.
6. **Bandwidth/cost** — continuous replication on cellular; need WiFi-only / metered policy.
7. **Device breadth** — per-device kernels/DTBs/vendor modules; start with one GKI device.
8. **Bootstrap trust of time** — TLS needs a sane clock before NTP.
