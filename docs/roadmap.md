# Diaspore — Roadmap (updated 2026-06-14)

> **The engineering build (Phases 0–4 below) is COMPLETE and proven on the Fairphone 3.** From here the
> single source of truth is the **Product roadmap** immediately below; the engineering phases are retired
> to build history.

Supersedes the netboot-centric plan in [phase1-spec.md](phase1-spec.md) after the 2026-06-09
model refinement: **the OS is stored locally; only user data roams.** Netboot-the-OS and
WiFi-in-initramfs are demoted to an optional stretch.

## Product roadmap (the active plan)

Business model: **free OS + turnkey devices + zero-knowledge roaming storage** — sold as **prepaid
bearer credits, *not* a subscription account** (an account would re-add a persistent identity; see
the billing model). Security/vertical market, Murena-shaped (deGoogled phone + paid
storage) but accountless. The OS is the loss-leader; the **roaming store is the revenue.**

### Phase 1 — FP3 product (productionize) · IN PROGRESS
Finish the shippable FP3/LineageOS product. Arc 1 (blind-login gate/identity) and Arc 2 (per-user
roaming) are done + proven on HW; what remains is productionization, organized as tiers:
- **Tier 1 — crypto correctness** ✅ — recovery key (data key wrapped under a pass-KEK + a recovery-KEK),
  passphrase rotation (`recover` via a 12-word code), and a signed/rollback-protected head (DIA-...-39).
- **Tier 2 — store config + roam-to-any-device** ✅ — on-device **Settings/Store** screen, a baked default
  endpoint, and **discovery/bootstrap** (name+passphrase → sealed store-config); `discover` /
  `publishDiscovery` + the baked-conf bootstrap (DIA-...-55). See [enrollment.md](enrollment.md).
- **Tier 3 — enrollment + ops safety:** enrollment rate-limit/gate, emergency-dial, scoped store
  credentials, rotate the demo Filebase key.
- **Tier 4 — self-service OTA:** an on-device updater service + a device-side delta chunk cache.
- **Tier 5 — branding/UX:** mostly done (DIA-20260613-10 — deep rebrand, icons, shutdown animation);
  the teal keyguard wallpaper + the boot "Phone is starting…" flash are minor OS-screen leftovers.
- **Tier 6 — reach (stretch):** GSI build; multi-store replication / traffic padding / warm offline
  cache; IPFS backend.

### Phase 2 — Commercialize · managed store + billing + storefront
Turn the product into recurring revenue. **One phase, built in two steps** (so there's no fractional
"Phase 1.5"):
1. **Validate willingness-to-pay** with a *simple* managed store + capability-token billing
   (pseudonymous + no-logs, even Stripe) — same gateway interface as the strong version.
2. **Zero-knowledge billing** — blind-token prepaid storage credits (see the billing model),
   the audited "we *can't* link you" upgrade.
Plus the public **website / storefront** (the old standalone "website" phase folds in here — it *is* the
commercial face). Depends on Phase-1 Tier 1 (recovery key) + Tier 2 (store config/discovery).

### Phase 3 — Endospore · GrapheneOS/Pixel "Secure Edition"
A second, higher-assurance device (Pixel + GrapheneOS) for the security/vertical market — rides the same
roaming store + prepaid credits. *(Reordered after Commercialize: prove revenue on the FP3 product before
building a second device.)* **Kickoff E.0 (2026-06-19):** spec + `editions/endospore/` scaffold landed;
target **Pixel 6/7**; build (E.1+) gated on the device. Detailed plan: **[endospore-spec.md](endospore-spec.md)**
(E.0→E.4 ladder, device matrix, GrapheneOS build env, relock + secure-element story).

### Phase 4 — FP6 · hardware refresh
Newer Fairphone (FP6) consumer turnkey refresh, once the model is proven.

### Phase 5 — Diaspore-native device · own the root of trust / green verified boot (north-star, revenue-gated)
A phone *made for this OS*. **The motive is owning the root of trust — nothing else.** On the FP3 we're
stuck at *yellow* (our AVB key, not the OEM's *green*); on Pixel/GrapheneOS (Endospore) we get locked
verified boot but **Google** owns the hardware key provisioning + secure element. A device where **we** own
the secure element + key provisioning is the only path to true **green** verified boot under our key and
hardware-bound key custody. That — not a faster or prettier phone — is the whole reason a security product
eventually wants its own device.
- **Feasible only in the ODM sense, not custom silicon.** Custom silicon = $10s–100s of M / years /
  hundreds of people (❌, ever). The realistic route is license an existing SoC platform (Qualcomm/MediaTek)
  + an ODM reference design, own the firmware / branding / **AVB root** — Fairphone / Purism / PinePhone
  scale (~$1–10M, MOQ in thousands, a small HW team, ~2 yrs, real inventory risk).
- **Pragmatic path to the same end:** deepen **co-design with an existing maker** (Fairphone or an ODM) to
  progressively gain control of the AVB root + a hardware kill-switch, rather than fabricating a phone —
  ~80% of the benefit for ~1% of the cost.
- **"Better than Endospore" = better at *our thesis*** (amnesiac roaming + verified boot under *our* root),
  **not** out-hardening GrapheneOS (years of hardened_malloc / mitigations — beating that is harder than
  building the phone).
- **Gated on** Phase-2 revenue + scale proving the model first. Until then this is a north-star, not a build item.

## The model in one paragraph

A Diaspore device runs an ordinary **local, verified, OTA-updated LineageOS**. Your *user
state* — app list, app private data, accounts, settings, files — **never persists on the
device**: it roams, encrypted, in a distributed store; restored on boot, replicated during
runtime, **wiped on power-off**. App *code* (public APKs) is **re-downloaded per device** from
its distributor (Aurora/F-Droid) and cached locally ([app-model.md](app-model.md)). Blind-login
a profile to re-materialize your phone on any Diaspore device; power off → blank slate.

## Positioning vs. the market

- **Not competing with Fairphone Easy.** That's *phone-as-a-service* (a sustainability / circular-economy
  lease — repairs included, cheaper the longer you keep it) running **stock Google Android**. No privacy or
  data-roaming angle — orthogonal to us; we could even *deliver* Diaspore via a lease like it.
- **Closest comparable = Murena / /e/OS** ("Murena-shaped" above): a **deGoogled** phone + an **encrypted
  cloud account** (Murena Workspace), with data **persisting** on the device. Strong, but account-based +
  persistent.
- **Diaspore's two novel axes — combined by nobody in market:** **amnesiac** (nothing at rest on the
  device; power off = blank slate) + **accountless / roaming** (blind login, no identity to subpoena; refs
  are unguessable hashes; the store can't even *enumerate* which profiles exist). Closest *individual*
  pieces: **Tails** (amnesiac, but desktop, no roaming) and **Murena** (deGoogled cloud, but account +
  persistent).
- **One-liner:** everyone else ships *"deGoogled phone + encrypted cloud account."* Diaspore ships
  *"amnesiac phone + accountless, roaming, zero-knowledge state."*
- **Honest caveat — different ≠ better for everyone.** Amnesiac costs real UX (full re-restore on boot,
  connectivity dependence for first boot, forgotten-passphrase → lost data until the recovery key). That's
  a **feature for the security / privacy / data-residency vertical** and **friction for the mainstream** —
  which is exactly why the plan targets the vertical, not a head-to-head with Murena for mass consumers.

## Layers

| Layer | Sensitivity | Where it lives |
|---|---|---|
| **OS** | public, immutable | stored locally; dm-verity; content-addressed OTA |
| **App code** | public | re-downloaded per device (Aurora/F-Droid), cached locally |
| **Private state** (app list, app data, settings, files) | the secret | encrypted roaming; restored on boot; wiped on power-off |
| **Regenerable** (dexopt, caches) | derived | regenerate / non-secret local cache |

## What's proven — and kept

- **Phase 0** ✅ — loop logic in a portable sim (13/13).
- **M1** ✅ — stock Cuttlefish boots on the dev VM (Ubuntu 24.04 + userns fix).
- **M2** ✅ — custom stage-1 **init interposition** (our `/init` runs PID-1, full boot) and
  **amnesiac `/data`** proven across a power-cycle.
- **M3 NBD / fetch-to-cache scripts** — kept as reference for the optional Phase 5.

## Refined boot / run / shutdown loop

- **Boot:** local **verified** OS boots → blind-login **chooser** (profile + passphrase → keys)
  → `/data` mounted as an **ephemeral** writable layer → Android userspace starts.
- **Restore:** a **roaming agent** (Android userspace, network up) restores the private state —
  app manifest → re-download missing app **code** (Aurora/F-Droid) → **install-then-restore-data**
  → settings/files; **working set first**, stream the rest.
- **Run:** the agent continuously replicates **encrypted private-state deltas** to the store.
- **Shutdown:** freeze → final delta push → verify retrievable → **wipe** the ephemeral `/data`.

> Key simplification: the restore uses **Android-userspace networking** (WiFi works normally),
> because `/data` is `latemount`. No initramfs network stack, no WiFi-in-init, no OS netboot.

## Engineering build phases — ✅ COMPLETE (proven on FP3; build history)

> Retained for reference. These are the *technical* milestones (Phase 0–4), all proven on the Fairphone 3
> — distinct from the **Product roadmap** above, which is the active plan. (Phase numbers here do NOT line
> up with the product phases; e.g. engineering "Phase 4 = real hardware" is done, product "Phase 4 = FP6"
> is future.)

### Phase 2 — Roaming user-state (THE CORE) · on Cuttlefish
- **P2.1** Ephemeral writable `/data` that still supports app install (evolve M2's `always_create`
  into a proper ephemeral/overlay `/data`).
- **P2.2** Roaming agent (Android-userspace service): restore-on-boot, replicate-at-runtime,
  flush + wipe-on-shutdown.
- **P2.3** Encryption + keys: passphrase → Argon2id → keys; restic-style chunked AEAD (the Phase 0
  model, made real, client-side).
- **P2.4** App model: app manifest + re-download (Aurora/F-Droid) + install-then-restore-data
  (see [app-model.md](app-model.md)).
- **P2.5** Lazy / working-set restore.
- **Exit:** on Cuttlefish — install apps + create data in session 1 → power-cycle → blind-login →
  apps + data return; power off → local state gone.

### Phase 3 — Profiles + OS lifecycle
- **P3.1** Chooser / blind-login UI (profile + passphrase) + hidden/duress profiles + per-profile policy.
- **P3.2** Verified local boot (dm-verity / AVB, custom key).
- **P3.3** Content-addressed OS OTA (publish OS content-addressed; device pulls deltas + A/B apply)
  — replaces netboot.
- **Exit:** multiple profiles; verified boot; OS updates via content-addressed OTA.
- Detailed plan: **[phase3-spec.md](phase3-spec.md)** (adds a P3.0 integration step + milestone breakdown, risks, decisions).

### Phase 4 — Real hardware
- **P4.1** Pick a GKI device; build/flash **Diaspore OS** (LineageOS + our stage-1 init + roaming
  agent). No netboot → a normal flash + the agent.
- **P4.2** Validate the full loop on the device.
- **Exit:** a real phone — local verified OS, roaming encrypted user state, amnesiac on power-off.
- Detailed plan: **[phase4-spec.md](phase4-spec.md)** (device selection + build→bake→AVB→OTA→flash ladder; the build is drivable, the flashing needs a physical phone).

### Phase 5 — Untethered netboot (OPTIONAL stretch)
- Only for **zero-pre-provision / kiosk**: netboot the OS over WiFi (the old M3 + M6). Reuses the
  `phase1/cuttlefish/m3-*.sh` NBD / fetch-to-cache work.

## Open questions

- **Restore timing:** how early can the agent populate `/data` (framework dependency)?
  Warm-cache offline boot vs cold first-boot-on-new-device.
- **Chooser placement:** pre-Android (framebuffer) vs an Android-level user/credential screen.
- **Play-app code** on an uncertified (unlocked) device — Aurora/microG limits.
- **dexopt:** regenerate vs non-secret local cache.
- **Key custody across the wipe:** passphrase-derived (Argon2id); recovery key.
- **Where user data lives:** client-encrypted, content-addressed, **swappable backend** — see
  [storage.md](storage.md) (default: S3-compatible object store; user-hosted or IPFS optional; sign
  the mutable head; replicate for availability).
- **Enrollment / Day-0:** how a profile is *created* (name+passphrase+recovery key) and how store
  config lives on an amnesiac device — see [enrollment.md](enrollment.md) (device-level config +
  default now; discovery/bootstrap indirection for seamless roaming; bind the ref to name+passphrase).
- **Device coverage:** how many OS images for "most devices" — see [portability.md](portability.md)
  (a few GSIs for reach + a handful of flagships for locked verified boot + optional LineageOS-ride;
  keep Diaspore's footprint in `/system` to maximize GSI reach).

## Remaining work

The consolidated checklist of everything designed-but-not-built lives in **[backlog.md](backlog.md)**
— storage/sync (incl. continuous background delta-sync + chunk-addressed efficient push), enrollment
(Create UI, recovery key, ref-binding), the Phase 4 ladder (P4.2c sepolicy → P4.3 AVB → P4.4 OTA →
P4.5 flash), and the GSI/portability track.

## Immediate next step

**Phase 2 — Commercialize (step 1: managed store + capability-token billing).** Phase-1 Tiers 1 (recovery
key) + 2 (store discovery) — the Phase-2 prerequisites — are **done**, so commercialization is unblocked.
See the build plan (gateway architecture + the token/lease interface + the
`nowhere-cloud` skeleton + slice breakdown); the *why* + the blind-token end-state is in
the billing model. Remaining Phase-1 polish (Tiers 3–6) is tracked in [backlog.md](backlog.md).
