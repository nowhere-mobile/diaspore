# Diaspore — Build Backlog (remaining work)

Status: living checklist · updated 2026-06-30

The single "what's left" list. Detail lives in the linked design docs; this is the index so nothing
designed-but-not-built gets lost. `[x]` done · `[~]` in progress · `[ ]` to do.

> **2026-06-11 session:** built + flashed the Diaspore FP3 image and hardened it end-to-end on real
> hardware — boot animation, "Welcome to Diaspore", roaming restore + on-boot auto-restore, continuous
> sync, item-level dedup, and a confined **enforcing** SELinux domain. Items below reflect that.
>
> **2026-06-23/24 session:** **lock-model L1–L5** built + verified on FP3 (session keyguard, warm resume,
> idle/failed-attempt auto-logoff, per-profile idle); the **core rename** (`diaspore*`→`nowhere*`) shipped;
> gate fixes (#27/#28/#29); the **gate-brand** sepolicy now actually bakes (`ro.nowhere.brand`→ gate reads
> "diaspore"); a **store-settings "✓ Connected to <store>"** banner; and **dev adb pre-auth** wired
> (`PRODUCT_ADB_KEYS`). The checkboxes below are updated to match.
>
> **2026-06-30 session:** the **Commercialize (Phase 2)** arc hit a live milestone — the **unlinkable
> blind-voucher subscription** model and the **free tier** (5 GB, cleared after 90 days unused) are built
> across `nowhere-cloud` (gateway + storefront) and `core/agent`, and the **gateway is deployed to
> production** (`api.nowhere.mobile`). On-device proof of both rides the next agent build. The new
> **Phase 2 — Commercialize** section below tracks it (the arc previously had no backlog presence).

## Phase 2 — Commercialize (managed store, billing, subscriptions)  (commercialize.md)

The least-knowledge managed store + zero-knowledge billing. Gateway + storefront live in the private
`nowhere-cloud`; the device half stays auditable in `core/agent`. Market/legal posture in
billing-model.md + payment-legal.md (held back from the public mirror).

- [x] **Control plane + billing client + GC** — the 4-endpoint gateway (`/quote` `/tokens` `/lease`
      `/cap`), the roaming token **wallet**, lease/footprint, and the rent-lapse **GC + grace**
      (DIA-20260618-12..15; gateway in `nowhere-cloud`, client in `core/agent`).
- [x] **Cap-gated I/O (least-knowledge writes)** — a managed device holds **no store creds**: reads are
      free `/cap`, writes go through a token-paid `/lease` + per-ref WriteCaps. Proven on FP3.
- [x] **Zero-knowledge blind-token issuance** — RFC 9474 blind RSA (stdlib-only, no circl): the device
      blinds a token locally, the gateway signs it **without seeing it**, the device unblinds → usage is
      unlinkable from payment (`pay → claim code → blind issue → redeem`).
- [x] **Unlinkable blind-voucher subscriptions** — `/sub/credit` (storefront, shared-secret) →
      `/sub/voucher` (device, subkey-authed blind vouchers) → `/refill` (anonymous, burns vouchers →
      spendable tokens): two unlinkable blind hops, auto-refill once per 30-day epoch. Built end-to-end and
      proven on FP3 (chooser Add-credits + QR scan = phone #183/#184); live on the gateway.
- [x] **Streaming cap-mode capFlush (#45)** — the seal spills sealed blobs to disk and PUTs them
      **streamed** (RAM bounded to one chunk) with live progress + an atomic head, fixing the
      multi-GB-session **OOM** and the **false ERR-TIMEOUT** "Not backed up" (phone #185, DIA-20260630-19).
      On-device validation rides the next build.
- [x] **Free tier — 5 GB, cleared after 90 days unused (#54)** — the gateway grants **token-less** leases
      per pseudonymous `profileRef` (`ledger.GrantFree`; 90-day TTL renewed on access; the existing GC reaps
      on lapse), zero-knowledge intact. The device falls back to a free lease when the wallet can't cover a
      write (`capFlush`) and renews on access (`payRent`); the storefront states the policy. Cloud #53/#54/#55
      + phone #186/#187 (DIA-20260630-20..24). **Gateway deployed to production 2026-06-30** —
      `api.nowhere.mobile/quote` advertises `free_gb`.
- [~] **On-device proof against the LIVE gateway** — the device-side subscription + free-lease code rides
      the **next agent build** (after the in-flight #45/P3c image); then prove a free user seals ≤5 GB
      cap-only and a subscriber auto-refills against `api.nowhere.mobile`.
- [ ] **Postgres ledger + concurrency threshold** — move the JSON-snapshot ledger to Postgres for
      durability + load (free-tier leases add ledger volume); load-test the concurrency threshold (task #53).
- [ ] **Production payment rails** — Stripe closed-loop prepaid, parked pending the entity / counsel step
      (token issuance stays DISABLED until then); anonymous rails later.
- [ ] **Storefront follow-ons** — subscription management (status / cancel) + QR claim/voucher-flow polish.

## Naming — neutralize the shared core (`diaspore*` → `nowhere*`)
- [x] **Rename the shared `core/` engine off the edition name — DONE (DIA-20260621-04; both editions; FP3-revalidated 2026-06-23).** `core/`
      is shared by BOTH editions (Diaspore/Fairphone + Endospore/Pixel) but is branded after one of them
      (`diaspore_agent`, `DiasporeChooser` / `com.diaspore.chooser`, the `diaspore`/`diaspore_chooser`/`diaspore_exec`
      sepolicy types, `/data/diaspore`, `/system/etc/diaspore`, `ro.diaspore.*`, `DIASPORE_*` env/conf keys,
      `diaspore-ota-version`, `diaspore.rc`, `diaspore_{login,provision,roamd,otad,gate}.sh`). Rename to the neutral
      **umbrella brand `nowhere*`** (NOT `endospore*` — that's just as edition-specific). Keep **user-visible labels
      per-edition** via overlay (gate says "Diaspore" on FP3, "Endospore"/"nowhere" on Pixel) — separate, cheap.
      **Do as its own dedicated PR, not bundled into E.2:** invasive (the package rename is load-bearing for
      device-owner provisioning, the `AdminReceiver` component ref, `seapp_contexts`, the init socket, every script);
      `DIASPORE_*` env/conf keys mean re-provisioning confs; `/data/diaspore` paths thread through agent + scripts +
      the tmpfs mount. **Must be re-validated on the SHIPPING FP3** (full roam cycle) as well as the Pixel — so
      schedule it *after* the E.2 roam proof, when both devices can be re-tested in one pass.

## Storage & sync — the roaming data path  ([storage.md](storage.md))
- [x] **S3 backend in the agent** — works with any S3 store; validated on Filebase/Sia, and end-to-end on the FP3.
- [x] **Continuous background delta-sync** — the **login daemon** periodically seals the LIVE roamed user
      (`/data/user/N` + `user_de` + `media`) via the su:s0 worker's `out` op (no reboot), serialized with
      login/logout (`workerMu`), every `DIASPORE_SYNC_INTERVAL`s (default 120). **Replaces the Arc-1
      `diaspore_sync.sh`**, which keyed off the tmpfs `.session` marker + pushed the working-set dir — both
      wrong for the per-user model, so it skipped every cycle (`[sync] no session`) and the roamed user's
      data was sealed ONLY at logoff. Now an unclean power-off loses at most one interval. (DIA-20260613-18.)
- [~] **Efficient push** — DONE for whole-item: per-identity **convergent sealing** (HMAC nonce) + skip-if-exists,
      so unchanged items dedup instead of re-uploading (validated on HW). REMAINING: **chunk-level CDC** so a
      *changed* large item re-uploads only its changed chunks.
- [x] **No-change logoff speed (local crypto)** — after the blob cache (DIA-20260625-08) killed the per-chunk
      network stat, a confirmed-clean seal still re-ran the FULL local crypto (re-chunk + zstd + encrypt + sha256
      over the whole ~2.6 GB tree, ~27 s) even when nothing changed. A **content cache** now maps the cheap
      plaintext-chunk-hash → the sealed-blob-hash (per store+DK, RAM/tmpfs `$STATE`, seeded at restore from the
      decrypted chunks → warm from login), so an unchanged chunk reuses its sealed form and skips pack+encrypt+post
      — only the read+CDC-chunk floor remains. SAFETY: a reuse fires only when `blobCacheKnown(sealed)` ALSO
      confirms the blob is in the store (a wrong skip = data loss), and the key is bound to the manifest version
      (V1 raw vs V2 zstd never collide). Unit-tested: no-change reuse + byte-identical restore across a simulated
      login→logoff process boundary, the missing-blob safety gate, version isolation, `-race`. (DIA-20260626-01.)
- [x] **Full-amnesiac mode** — `/data/diaspore/state` is **tmpfs (RAM)**, mounted by init at `boot_completed`
      (NOT `post-fs-data` — that corrupts /data → bootloop), `context=`-labeled diaspore_data_file. Gone on any
      power-off (clean OR unclean) = nothing roamable at rest. **Verified on FP3 (enforcing):** a local-only
      marker vanished on reboot while alice's `1-home` roamed back from the store; restore + sync `rc=0`, zero
      real denials. Plus a **sync guard** (skip push when state is empty) so a failed restore can't clobber the
      store. (tmpfs is RAM-bound — fine for the working-set; full app data is the bigger /data story.)
- [x] **App-data wiring → the PER-USER model — DONE** (Arc 2, proven on FP3): each identity roams its full
      `/data/user/N` (telephony/SMS, contacts, call log, every app's `/data/data`) + `/data/user_de/N` +
      `/data/media/N`, each **CDC-chunked** (bounded RAM — no longer the `/data/diaspore/state` placeholder), so a
      restored profile has its messages / calls / apps. **See [roaming-boundaries.md](roaming-boundaries.md)** for
      what roams (data) vs what can't (device-bound trust: Keystore, Play Integrity, server registration —
      WhatsApp/Signal login, banking, DRM don't roam).
- [x] **Sign + rollback-protect the mutable head — DONE** (2026-06-15, DIA-20260615-39): the vault head now
      carries a monotonic **`version`** (bumped on every push/rotate) + a **`sig`** = HMAC over the header
      keyed by a DK-derived key. On restore the agent **verifies the signature** (a signed head that doesn't
      verify → reject = a leaked-key store can't forge/alter a head without the passphrase); legacy unsigned
      heads are skipped (backward-compat, re-signed on the next push). **Rollback enforcement is OPT-IN**
      (`DIASPORE_ROLLBACK_ANCHOR=1`): a persistent `profileRef → highest-version` anchor in `/data/diaspore/rollback`
      (survives the wipe) rejects an old-head replay — **off by default** because the anchor records (as
      unguessable hashes) that profiles logged in here, trading plausible deniability (a duress concern). Verified
      on VM (sign / version / tamper-reject / rollback-detect) + FP3 (legacy ltest vault logs in, re-signs on push).
- [ ] Multi-store replication (read-from-fastest); size-bucket/pad chunks (traffic analysis); encrypted
      warm cache for offline boot.
- [ ] (stretch) IPFS blob backend + signed-ref/IPNS head (fully decentralized story).

## Enrollment & identity — "Day 0" + login UX  ([enrollment.md](enrollment.md))
- [x] **Blind-login chooser + HARD GATE — WORKS + BAKED on the FP3 (enforcing).** Confined `diaspore_chooser`
      app → AF_UNIX socket → root `diaspore_agent login-daemon` → in-memory restore; correct creds unlock +
      restore, wrong passphrase → identical blank. **(5b) HARD GATE done:** the chooser IS the device HOME — boot
      lands on the gate (empty tmpfs, no auto-restore), unlock hands off to launcher3, HOME passes through when a
      session is active, and a kiosk lockdown blocks the shade/recents/back (no escape). Proceed-to-home replaced
      the `N item(s)` readout. Verified running from `/system` (committed 32e777c). **Fresh-flash self-provision
      DONE (DIA-20260615-42 review):** the old "default-home ROM preset" note is **stale** — after the dynamic-home
      flip (below) there is no chooser-as-home adb setting: launcher3 is HOME and the gate is launched **over** it by
      `BootReceiver` + the `diaspore_gate` init service (every boot), while `diaspore_provision.sh` (init `late_start`,
      idempotent per fresh `/data`) self-provisions the **device owner** + keyguard-off with **zero adb**. So a clean
      flash boots straight to the kiosk gate on its own.
- [~] **Gesture-proof KIOSK + DYNAMIC HOME** — the device-owner **Lock Task** kiosk WORKS (verified on HW: boot →
      gate, `lockTaskModeState=LOCKED`, the gesture-nav **overview / app-cards escape is blocked**). Needed a factory
      reset to clear a stuck owner, then a CLEAN device-owner provision for the un-updated `/system` APK
      (`isDeviceOwnerApp` desyncs if you `adb install` the app *after* provisioning). Also fixed a real crash: a
      physical button tap → click sound → needs `audio_service` (granted, + `device_policy`/`statusbar` for the
      lock-task code). **(a) DYNAMIC-HOME — DONE** (committed bbe0945): flipped it — **launcher3 is the persistent
      home** (gestures/app-drawer/search work), the chooser drops the HOME category and is a **BootReceiver-launched
      Lock-Task gate** that `finish()`es on login to reveal launcher3. A `/system` flash KEEPS the device owner (no
      desync, unlike `adb install`), so rebuild+flash is the iteration path. Verified on HW: gate + LOCKED at boot,
      login → launcher3, drawer/search work. **(b) DONE (DIA-20260615-42 review):** device-owner provisioning is
      **in the ROM** — `diaspore_provision.sh` runs at first boot per fresh `/data` and `dpm set-device-owner`s the
      chooser (no adb); no "default home" step is needed (launcher3 is home, gate launched over it). REMAINING: only
      (c) the ~1-2s boot-gap where launcher3 can flash before the gate (minor cosmetic).
- [x] **Chooser "Create" path — DONE** (committed 055ddcb): gate has a New-profile toggle (name+pass+confirm); daemon
      `CREATE` seals an empty manifest under (name,pass) + logs straight in → launcher3. **profileRef now bound to
      (name,pass)** — a wrong pass derives an unguessable ref that resolves to nothing (tighter blind login), and CREATE
      refuses an existing (name,pass) with `EXISTS` without leaking which *names* exist. `restoreSet` returns
      (items, **valid**) so an empty 0-item profile still counts as a login; a non-dir `.session` marker (skipped by the
      subdir-only sync → never roams; cleared by the tmpfs wipe) makes STATUS=ACTIVE for it. One-time `migrate-ref`
      re-points old name-only refs (ran for alice; 1-home intact). Verified on HW. **FOLLOW-UPS:**
      **Enrollment rate-limit DONE (DIA-20260615-41):** open enrollment let anyone at the gate mint arbitrary
      (name,pass) profiles → store pollution; now a per-device **token bucket** throttles CREATE — capacity
      `DIASPORE_ENROLL_MAX` (default 5), fully refilled over `DIASPORE_ENROLL_WINDOW`s (default 3600), state at
      `/data/diaspore/enroll` which **survives the power-off wipe** (like the rollback anchor + conf) so a flood
      can't be power-cycled away. The gate shows "too many new profiles — try again in N min"; `MAX=0` disables
      it for turnkey devices. This is a LOCAL (one-device) throttle; the cross-device / store-side enrollment
      gate stays a Phase-2 broker (ties to scoped store creds).
      **Emergency-dial DONE (DIA-20260615-40):** a red "Emergency call" link on the gate launches the system
      emergency dialer (`com.android.phone/.EmergencyDialer`); `com.android.phone` is allow-listed for Lock
      Task so 911 works from the LOCKED gate without dropping the kiosk (verified on FP3: dialer launches,
      LockTask stays LOCKED, BACK returns to the locked gate). **Info-card + naming follow-on (same ID):**
      allow-listed `com.android.emergency` so the gate's "Emergency information" card opens its viewer in-kiosk
      (was silently Lock-Task-blocked) and the View/Edit stay contained in that package (no Settings/launcher
      escape); named primary user **"Diaspore"** (was the generic "Owner" on the card). Verified on FP3.
- [x] **CREATE roam-in `unexpected EOF`** — FIXED (DIA-20260613-11): a DIA-05/CDC regression where
      `handleCreate` sealed the head as `seal(key, json(Manifest{}))` but roamd's roam-in `restore` expects
      a `cdcMagic` chunk-manifest or a legacy tar → it untarred the JSON → `unexpected EOF` → a freshly-
      CREATEd profile bounced back to the gate. Fix: `handleCreate` seals an **empty tar** (restore's
      legacy-tar branch untars it to 0 files = valid empty session). Proven on FP3 (`created demo` →
      `IN demo -> user 10`, lands on home). NOTE: old-format heads predating the fix (e.g. alice) still need
      a re-seal — a fresh CREATE is clean.
- [~] **Session lifecycle — logoff DONE** (committed 2f4a8f2): a "Log off" launcher entry → daemon `LOGOFF` → final
      sync of the live session → wipe the tmpfs → back to the gate (data *leaves* the device, no reboot, stronger than
      a lock screen). Required a **session-driven sync** rewrite — the `.session` marker now carries name+pass and the
      continuous sync reads it instead of a baked profile; the old baked sync was clobbering other identities (caught it
      emptying alice during the Create test). "switch profile" already works as logoff → gate → login another identity.
      **REMAINING:** full **Android multi-user** staging (each identity = an ephemeral user; Android tears down/re-inits
      the session; Diaspore hooks the user lifecycle) for true per-user isolation — ties to app-data-wiring. Makes
      Diaspore an any-identity / shared-device / duress-switch terminal.
- [x] **No-reboot logoff (UX) — DONE** (2026-06-15, DIA-20260615-31). Logoff used to **reboot** to wipe the
      ephemeral roamed user + restore the gate. Key finding: `am stop-user -f -w N` on the ephemeral user
      **removes it AND deletes `/data/user|user_de|media/N` immediately** (no husk-until-reboot; FBE destroys
      the user CE key → equivalent to the reboot-wipe). BUT the teardown's `am switch-user`/`am stop-user`
      **only work from a device-owner caller** — the su:s0 worker hits the device-owner wall (`Failed
      transaction`); a spike that tested via adb-root was misleading (the shell pkg carries
      `MANAGE_USERS`/`INTERACT_ACROSS_USERS_FULL` the worker lacks). So the teardown runs in the **user-0
      chooser** (device owner): a daemon `LOGOUT` verb seals + clears `.roamsession` + queues a pending-reap
      uid; the chooser's user-0 process **polls `POLL-REAP`** (~1.5s) and, on a hit, `switchUser(0)` +
      relaunch-gate + `dpm.stopUser(N)` (the accept loop is serial, so the daemon can't push — hence polling).
      `LogoffActivity` sends `LOGOUT`; an 8s daemon fallback reboots if no chooser is polling. `ROAM-OUT`
      (reboot) retained as the optional **"secure logoff"** that also clears all in-RAM system state.
      **Unescapable logoff DONE (2026-06-16, DIA-20260615-46):** the "Logging off…" screen was dismissable
      (Home/recents dropped you back into the session being sealed+reaped — you could keep using a session
      you'd "logged off"). Now once logoff starts, `LogoffActivity` **swallows Back** and **re-fronts on any
      Home/recents** (`onUserLeaveHint`) until the reap switches to the gate. The gesture-nav recents/overview
      swipe doesn't fire `onUserLeaveHint`, so re-front alone can't catch it — **closed instead by a two-phase
      reap (DIA-20260616-49):** `handleLogout` queues a **SWITCH** the user-0 chooser does **before** the S3
      seal (foreground back to the gate, backgrounding the roamed user), then seals the still-running user, then
      queues the **REAP** (remove) — so even a recents swipe gets yanked back to the gate almost immediately and
      the session is gone, while seal-before-wipe is preserved (remove only after seal). Unit-tested
      (`nextReapAction` SWITCH→REAP, once each). **Escape window shrunk ~1.5s → ~0.12s (DIA-20260618-07, proven
      on FP3):** two compounding causes — (a) while a session is foreground the user-0 reap-watcher process is
      `cch-empty` → **frozen by the cached-app freezer** so its poll lagged ~1.5s no matter the interval, and
      (b) **`PowerManager.isInteractive()` reads FALSE from that backgrounded user-0 process even screen-on**, so
      a screen-gated fast-poll never fired. Fixed by making the platform-signed chooser **`android:persistent`**
      (PERSISTENT adj, never frozen) + gating the watcher's 250ms fast-poll on a **`sessionLive`** flag (set at
      login, cleared after the reap) instead of the screen state; idles to 15s only when parked at the gate.
      **Lock-Task was a DEAD END (DIA-20260616-48,
      reverted):** it needs device-owner **affiliation**, but a DO with affiliation IDs forces its *unaffiliated*
      roamed users through the **SetupWizard on login → roam-in BREAKS**; affiliation is now actively **cleared**
      on the gate (self-heal). VERIFY the two-phase reap on HW (login → logoff → try the recents gesture).
- [x] **Remove baked `DIASPORE_PROFILE`/`DIASPORE_PASS` from diaspore.conf — DONE** (DIA-20260613-22 + the
      2026-06-14 security pass): they were the only consumers of a passphrase-at-rest; stripped from `diaspore.conf`
      + the backup, and the dead Arc-1 scripts (diaspore_boot/shutdown/sync) that read them were removed.
- [x] **Recovery key + passphrase reset — DONE** (2026-06-14, DIA-20260614-23..28): wrap a random Data Key under a
      passphrase-KEK + a recovery-KEK (keyslots / LUKS model); enables recovery via a **12-word BIP39 code** AND
      **instant passphrase rotation** (no data re-encryption — DK is unchanged); existing profiles auto-migrate on
      next login. Proven end-to-end on FP3 (create→code, recover-at-gate, change-passphrase). Design: **[recovery.md](recovery.md)**.
- [x] **`delete-profile` verb — DONE** (2026-06-15, DIA-20260615-29): `diaspore_agent delete-profile <store> <name> <pass>`
      drops the head + recovery refs so a name no longer resolves (blobs left for store GC). Used to clean throwaway
      test profiles; the product's "delete my profile" primitive.
- [x] **User-facing "Delete my profile" affordance — DONE** (2026-06-16, DIA-20260616-50): the LogoffActivity
      confirm screen now carries a destructive (red) **"Delete this profile"** link → a confirm screen with the
      profile name, a permanent-removal warning, and a passphrase field → daemon **`DELETE\n<name>\n<pass>\n`**.
      Shared `deleteProfile(base,name,pass)` backs both the `delete-profile` CLI verb and the new daemon `DELETE`
      verb; the typed pass re-authenticates (wrong pass → `NOTFOUND`, nothing deleted). On `OK` the daemon drops
      the head + recovery refs from the store and reaps the local session **without a final seal** (two-phase
      reap, both phases queued at once, so the deleted profile is never re-uploaded), then the user-0 chooser
      switches to the gate + removes the ephemeral user. 8s reboot fallback if no chooser is polling.
- [x] **Ref binding to name+passphrase — DONE** — `profileRef(name, passphrase)` is a hash of BOTH (unguessable,
      collision-free, leaks nothing); the recovery vault adds a second unguessable `recoveryRef(name, entropy)`.
- [~] **Store config (Tier 2)** — **device-level config + discovery DONE** (2026-06-15, DIA-20260615-35/36/37/38):
      the login daemon starts even with no conf and exposes `GET-STORE` / `SET-STORE` / `TEST-STORE`; a chooser
      **Settings/Store** screen sets the S3 endpoint/bucket/region/creds in-place (applied live), with a first-run
      "no store set" hint + a "Test connection" check. **Discovery/bootstrap (DIA-…-38):** `bootstrapRef`/`bootstrapKey`
      from name+passphrase; on CREATE + each login the daemon **auto-publishes** the sealed store-config to a baked
      **discovery endpoint** (DISCO_* / discovery.conf), and a fresh device with no store **auto-discovers + applies**
      it on a login — so a profile re-materializes on any Diaspore device from name+passphrase alone. Verified
      end-to-end on FP3. So a fresh free-OS flash configures its store on-device — no manual `scp`, and no re-entering
      creds when roaming. **Turnkey baked default DONE (2026-06-15, DIA-20260615-42):** `diaspore.mk` now bakes a
      device-default `discovery.conf` into `/system/etc/diaspore/` **if `vendor/diaspore/etc/discovery.conf` exists at
      build time** (`$(wildcard …)`), so a freshly flashed / factory-reset device boots to the self-provisioned gate
      AND bootstraps `name+passphrase → your phone` with **zero manual store setup** (the daemon already falls back to
      the `/system` copy + auto-discovers on login). The real file is **`.gitignore`d** (only `*.example` is tracked),
      so a **clean checkout still builds an un-enrolled OS** — creds never enter git or a default build. REMAINING: a
      **production discovery backend** (least-privilege / public-read / managed least-knowledge gateway — today's dev
      bakes an account-level key; the Phase-2 token broker scopes it).
- [x] **Wi-Fi onboarding at the gate — DONE** (2026-06-15, DIA-20260615-43): a true factory-reset demo of the
      turnkey path surfaced the real gap — a freshly wiped **wifi-only** device self-provisions to the gate fine, but
      its saved wifi went with `/data`, it has no SIM, and the **kiosk blocks Settings**, so it can never get online to
      reach the baked discovery (`tryDiscover` → `NOSTORE`). Fix: a **"Wi-Fi" link on the gate** opens a **contained**
      in-app connect screen (SSID + password → the chooser, as **device owner**, `addNetwork`/`enableNetwork` via
      `WifiManager` — DOs are exempt from the API-29+ self-config block). It is an in-app view swap, **not** a launch
      into Settings, so the Lock-Task kiosk is never dropped and there's no escape (no allowlist change). Closes the
      turnkey loop: fresh flash → gate → **Wi-Fi** → join → `name+passphrase` → your phone.
- [x] **Scanned Wi-Fi picker — DONE + VERIFIED on FP3** (2026-06-16, DIA-20260615-45): the gate Wi-Fi screen scans
      + lists **tappable nearby networks** (strongest-first, deduped, 🔒 = secured); tap fills the SSID + focuses
      the password. Manual field stays as the fallback. **GOTCHA (cost a long debug, cracked via adb logcat):
      `NEARBY_WIFI_DEVICES` alone is NOT enough on this LineageOS 22.2 / Android 15 build** — `getScanResults()`
      silently returns empty and `startScan()` returns false, with no AVC denial and no log. The device owner must
      ALSO self-grant **`ACCESS_FINE_LOCATION`** (+ `setLocationEnabled(true)`); then `scan=1 n=44 → 7 ssids`.
      Verified on HW (logcat `owner=1 fine=1 near=1 wifi=on scan=1`). **Privacy grant-then-revoke DONE**
      (2026-06-16, DIA-20260616-52): the gate holds the scan's location access only while the Wi-Fi screen is
      open, not at rest. `scanWifi` grants `ACCESS_FINE_LOCATION` + `NEARBY_WIFI_DEVICES` + enables location for
      the scan; `revokeScanPerms()` hands them back (`setPermissionGrantState(... DENIED)` + `setLocationEnabled
      (false)`) when the user **leaves** the Wi-Fi screen (Back / after connect), and `onResume` revokes any
      **leftover** grant whenever the idle gate foregrounds (boot / post-logout). **GOTCHA:** the revoke can NOT
      go in `scanWifi`'s `finally` — revoking a runtime permission the app currently holds **restarts the
      process**, which killed the gate right after the scan and threw away the scanned-network list (verified:
      gate pid changed across the scan). Granting does not restart, so the grant stays inline; the revoke is
      deferred to screen-exit / onResume where a gate restart just re-shows the (idle) gate and self-heals.
      FINE_LOCATION stays *declared* in the manifest (only the runtime grant is dropped). Verified on FP3: gate
      boots `granted=false`; scan keeps the picker (pid stable, networks listed); reboot with a leftover grant
      → onResume revokes → `granted=false` + LOCKED, stable.
- [x] **Wi-Fi screen shows the current SSID when connected — DONE + VERIFIED on FP3** (2026-06-16,
      DIA-20260616-57): when already associated, the gate Wi-Fi screen shows **"✓ Connected to <SSID>"** (accent)
      in place of the "fresh device has no Wi-Fi" copy (status line → "tap another network to switch, or type one
      below:"); the picker + manual fields stay so the user can still switch. The SSID is location-gated
      (`getSSID()` → redacted `<unknown ssid>` without a grant), so a new `connectedSsid()` helper
      (`getConnectionInfo()`, deprecated but DO-allowed) is read **in the scan thread** while the scan's
      `FINE_LOCATION` grant is held → un-redacted; returns null when not associated (`netId == -1`) / still
      redacted. No new permissions, no sepolicy change; Back still `revokeScanPerms()`. Verified: "✓ Connected to
      Oviya" on an associated device, Back → gate LOCKED.
- [x] **Meaningful sign-in failure + no-internet pre-check — DONE** (2026-06-16, DIA-20260615-47): the gate's "—"
      blank was cryptic (and showed even when a login just timed out for lack of network, e.g. right after a reboot
      before wifi rejoins). Now: a **connectivity pre-check** (`hasInternet()`) shows *"No internet yet — tap Wi-Fi
      to connect (or wait for it to reconnect)"* instead of attempting a doomed restore; and a genuine failure shows
      a friendly but **UNIFORM** *"Couldn't sign in — check your name, passphrase, and Wi-Fi"* — wrong creds and an
      unknown profile still read identically, so blind-login / duress deniability is preserved (network state isn't
      secret, profile existence is).

## Phase 4 — real hardware (Fairphone 3)  ([phase4-spec.md](phase4-spec.md))
- [x] P4.1 vanilla LineageOS FP3 build (host + device + toolchain proven).
- [x] P4.2a agent + init hooks baked into /system; [x] P4.2b runtime wired to the S3 store via config.
- [x] **P4.2 Diaspore-flavored FP3 image** — agent + init hooks + conf + boot animation baked (targeted `m systemimage`).
- [x] **P4.2c sepolicy** — confined **enforcing** `diaspore` domain (dropped the `su:s0` placeholder); 0 AVC denials on HW.
- [x] **P4.5 flash + validate** — flashed on the physical FP3; on-boot restore + continuous sync verified end-to-end.
- [x] **P4.3 verified boot** under the Diaspore key — dm-verity re-enabled + custom-key AVB +
      `avb_custom_key` + bootloader **locked → yellow**, proven on the FP3 (DIA-20260613-07). Both
      slots flashed Diaspore (so an A/B fallback can't reach the stock slot). Runbook:
      [`../phase4/build/p4.3-verified-boot.sh`](../phase4/build/p4.3-verified-boot.sh). Follow-up: back
      up the custom AVB private key (now the device root of trust) securely off the build host.
- [x] **P4.4 OTA** — signed full A/B payload applied via `update_engine` on the **locked** device
      (no unlock), AVB-verified under our key (stays yellow), slot-switched, booted v2; A/B rollback
      both ways; `/data` untouched. Proven on FP3 (DIA-20260613-08). Runbook:
      [`../phase4/build/p4.4-ota.sh`](../phase4/build/p4.4-ota.sh).
  - [x] **P4.4b** content-addressed OTA transport — the OS payload roams through the store as CDC chunks
        (reusing the agent's `push`/`restore`, no new code): 780 chunks, fully deduped on re-push,
        CDC-restored **byte-identical** on the locked device, applied via `update_engine`, booted v2
        clean. Proven end-to-end on FP3 (DIA-20260613-09). Runbook:
        [`../phase4/build/p4.4b-ota-cdc.sh`](../phase4/build/p4.4b-ota-cdc.sh). **Gotcha:** never
        `rm`/`mkdir` `/data/ota_package` (system FBE-policy dir → `set_policy_failed` → boot fails);
        clear only the files inside.
  - [x] **Device-side chunk cache (DIA-20260618-02)** — the agent's CDC `restore` caches chunks by content
        hash (`DIASPORE_CHUNK_CACHE=<dir>`), so a later v2→v3 restore network-fetches only the changed
        chunks (true delta download); ciphertext-only (safe at rest), self-healing (hash-verified reads),
        off by default. The p4.4b runbook points it at `/data/diaspore/otacache`.
  - [x] **On-device updater service + user-confirm UX (DIA-20260618-03)** — `diaspore_otad` (su:s0) does the
        on-device half (Wi-Fi wait → `ota-check` vs the `/system` stamp [`ota-mark` publishes the version] →
        CDC-restore the payload, delta via the chunk cache → `update_engine` apply → reboot into the new slot),
        **no computer**. Trigger is **user-confirm at the gate**: the daemon's `OTA-STATUS`/`OTA-APPLY` back a
        gate "Update available (vX) · Install" link → tap → "Updating Diaspore…" → daemon `ctl.start`s the
        updater (labelled `ctl_diaspore_otad_prop`; no other sepolicy needed). **Proven E2E on FP3** (prompt →
        tap → apply 0→100% → self-reboot, clean, loop-guarded).
  - [x] **OTA settings — auto-install opt-in (DIA-20260618-06)** — "Check for updates" + the manual Install ship
        in the Settings gear (DIA-04). The optional **unattended** path is now done: a Settings → Software update
        **"Install automatically while charging"** switch (off by default) makes the gate apply a published update
        on the **next screen-off while charging** (never mid-use) instead of only the user's explicit confirm.
        Daemon `GET-/SET-OTA-AUTO` persist a device-level flag at `/data/diaspore/ota-auto` (survives reboot + the
        power-off wipe, factory-reset-cleared, never roams); chooser drives it via an `ACTION_SCREEN_OFF` receiver
        that re-checks toggle+charging+`OTA-STATUS AVAIL` → `OTA-APPLY` (reuses DIA-03's confirm path, no new
        sepolicy). Proven on FP3.

## Portability / reach  ([portability.md](portability.md))
- [ ] **GSI build** (Tier-1) — the amnesiac/roaming experience on most Treble devices (best-effort HW,
      unlocked bootloader). Keep Diaspore's footprint in `/system` to maximize this.
- [ ] A couple more flagship devices (a popular Pixel); optional automated per-device LineageOS builds.

## Branding / UX
- [x] **Gate settings screen (declutter the footer) — DONE (DIA-20260618-04)** — a top-right **⚙** opens an
      in-app Settings screen (a `setContentView` swap like the Store/Wi-Fi/Recover screens → stays inside
      Lock-Task) holding **Store settings**, **Wi-Fi**, and a **Software update** section (status + manual
      "Check for updates" + the user-confirm Install). The gate footer is now just the login flow + **Emergency
      call** (kept on the gate). An available update stays discoverable via an **accent-tinted gear**
      (`checkForOta`), so the footer "Install" link is gone. Proven on FP3. (Auto-install-while-charging added in
      DIA-20260618-06.)
- [x] **Boot animation** — Diaspore wordmark + dispersing-spore motif; **STORED** zip (mmap-able), installed via
      LineageOS's `TARGET_BOOTANIMATION` hook; confirmed playing on the FP3.
- [x] **Rebrand "LineageOS" → "Diaspore"** — DONE the deep sweep (DIA-20260613-10): SetupWizard
      ("Welcome to Diaspore"), **Model = Diaspore** (`PRODUCT_MODEL`), the About **"Diaspore version"**
      label (lineage-sdk `lineage_version` overlay), **LineageParts** titles, and a **neutral build
      identity** (`ro.build.user`/`host`/`version.incremental` no longer leak `chesterr` + the GCP build
      host). Verified on FP3. Residual `-FP3` codename suffix in `ro.lineage.version` and the
      recovery/Quick-Settings deep strings are intentionally left (technical/internal, not user-facing).
- [x] **Profile / login UI polish** — DONE (2026-06-11): restyled the unlock chooser to the Diaspore brand
      (dark canvas, "diaspore" wordmark + dispersing-spore motif drawn on a Canvas, teal accent, rounded dark
      blind-login fields, Unlock button, amnesiac footer; dark `Theme.Material.NoActionBar` so no light flash /
      title bar). Programmatic styling, no res/. Verified on FP3 (enforcing) — matches the mockup; unlock logic
      unchanged (correct→UNLOCKED, wrong→BLANK).
- [x] **Shutdown animation** — DONE (DIA-20260613-10): the disperse motif in reverse (gather + fade),
      STORED zip at `/product/media/shutdownanimation.zip`. The verified-boot **yellow-screen identity**
      (carries the Diaspore key, P4.3) remains — bootloader-level, not customizable on the locked FP3.
- [x] **Log-off / app icon** — DONE (DIA-20260613-10): `ic_diaspore.xml` spore motif (was the default).
- [x] **Durable keyguard-off + gate self-arm** — DONE (DIA-20260613-10): keyguard-off re-applied every
      boot (the one-time provisioning set didn't persist); a `diaspore_gate` init service `am start`s the
      gate at `sys.boot_completed` (clears the Android-12+ stopped-state so a cold device auto-arms).
- [x] **Gate kiosk stability** — DONE (DIA-20260613-10): the gate **swallows volume keys** — pressing one
      NPE'd (`MediaSessionManager` has no service in the confined domain), crashing the gate and dropping
      Lock Task (letting the shade/notifications through). Now `mLockTaskModeState=LOCKED` holds.
- [ ] **Boot home-flash (DEFERRED — architectural)** — minimized (early gate-launch + dark home wallpaper)
      but a sub-second blink remains: the OS boot-animation→first-app + "Phone is starting…" (FBE) window,
      inherent to launcher3-as-home. `directBootAware` and `persistent` were tried and reverted (made it worse
      / hid a crash). Unlike the **post-logoff flicker** (DIA-20260616-53, fixed — that one was a suppressible
      `CONFIG_ASSETS_PATHS` activity relaunch), this is the genuinely architectural sibling: the only real fix
      is making the gate the home again, a deliberate, hard-won revert. **Kept deferred on purpose** until/unless
      that tradeoff is revisited. **Keyguard branding DONE (DIA-20260617-05):** the keyguard (mostly disabled —
      the gate is the lock — but it shows for roamed ephemeral sessions on a screen-off) now carries the Diaspore
      identity via `setOrganizationName("diaspore")` + `setDeviceOwnerLockScreenInfo("diaspore · your phone,
      nowhere")` (device-owner scope, set on the gate) over the existing teal wallpaper. A full spore **graphic**
      lock wallpaper stays deferred: AOSP has no build-time default lock wallpaper, so it'd need a per-user
      runtime `FLAG_LOCK` set on each ephemeral roamed user (the per-user `BootReceiver` won't fire — the app
      sits `FLAG_STOPPED` for a fresh user until launched).
- [x] **Post-logoff flicker — FIXED** (2026-06-16, DIA-20260616-53). The one-frame flicker as "Signing out…"
      cleared was the **gate Activity RECREATING**, not a launcher3 frame (the alt cause). Diagnosed via adb
      logcat during a real logoff: removing the roamed user flips **`CONFIG_ASSETS_PATHS` (0x80000000)** on the
      gate's display (per-user asset-path reconfiguration), which relaunched `ChooserActivity`
      (`finishDrawing of relaunch … 183ms` = the visible flicker). Fix: `android:configChanges="assetsPaths"`
      on `ChooserActivity` — the gate's views are all programmatic (no per-user/overlay assets to reload), so
      it handles that config change itself instead of recreating. Verified on FP3: same `Config changes=8...`
      fires on the reap but the relaunch count drops 1 → **0**, gate stays up + `LOCKED`, no flicker.
- [x] **SetupWizard wins the boot foreground race → gate can't kiosk — DONE** (2026-06-16, DIA-20260616-51). On
      some boots `org.lineageos.setupwizard` reached the foreground before the gate, so the gate's
      `startLockTask` threw `IllegalArgumentException: Invalid task, not in foreground` → `state=0` (not LOCKED)
      → the gate never kiosked and SetupWizard stayed up ("welcome screen"), even on a provisioned device
      (device_provisioned=1, user_setup_complete=1). Fix: `diaspore_provision.sh` now disables SetupWizard on
      **user 0** (`pm disable-user --user 0 org.lineageos.setupwizard`, once per fresh `/data`, persists until a
      factory reset which re-runs provisioning) so it can never win the race — `prepRoamedUser` already disabled
      it for the roamed users; this covers user 0.
- [x] **Gate retries `startLockTask` once it is foreground — DONE** (2026-06-16, DIA-20260616-51). `lockGate(true)`
      was a one-shot; if it fired while the gate's task wasn't foreground the Lock Task silently failed
      (`state=0`) and was never retried, so the kiosk stayed down for that boot. Fix: `ChooserActivity` tracks
      `wantLocked`, and `onWindowFocusChanged(hasFocus)` re-runs `lockGate(true)` when the gate gains focus (i.e.
      is now foreground) and `getLockTaskModeState() != LOCKED` — idempotent, a no-op once LOCKED. Belt-and-
      suspenders with the SetupWizard fix. Verified on FP3: boot → gate → `mLockTaskModeState=LOCKED`. NOTE: do
      NOT `uiautomator dump` the gate to debug it — it crashes the gate (custom views blow up in accessibility
      traversal); use `screencap` + blind `input tap`.
- [x] **Gate scrolls when content overflows — DONE + VERIFIED on FP3** (2026-06-16, DIA-20260616-59). The gate's
      column was the content view with **no ScrollView** (unlike the Store/Wi-Fi/Recover screens), so a long status
      message (the 2-line "No internet yet — tap Wi-Fi…", worst in create mode / with the keyboard up) pushed the
      footer links (Store settings / **Wi-Fi** / Emergency call) off the bottom, unreachable (reported by Chester).
      Fix: wrap the column in a `ScrollView` + `setFillViewport(true)` — normal layout pixel-identical, scrolls on
      overflow. Verified: keyboard up → swipe → Create + Wi-Fi + Emergency call all reachable.
- [x] **Wi-Fi connect returns to the gate, not the Settings screen — DONE (shared-core routes connect-success → Settings; both editions). (reported on Endospore 2026-06-21; also seen on
      Diaspore)** — after connecting to Wi-Fi from gate → ⚙ Settings → Wi-Fi, the view drops back to the **gate**
      instead of the **Settings** screen it was opened from. Same class as the Diaspore **DIA-20260618-18** fix
      (Store Cancel / Wi-Fi Back → Settings, not gate). The chooser is shared `core/`, so either that routing
      didn't cover the Wi-Fi connect-**success** path (DIA-18 fixed Back/Cancel; a successful connect intentionally
      went to the gate with a "sign in now" hint) or it regressed. Decide the intended landing after a Wi-Fi
      connect — **Settings** (keep configuring: enter Wi-Fi, then set up the store) vs the gate (sign in now) — and
      route `buildWifiScreen`'s success path there (likely Settings, mirroring DIA-18). Chooser-only; affects both
      editions.
- [x] **CREATE recovery-key screen has no "working" feedback during user creation — DONE ("Creating your space…" status + disabled button while createRoamUser runs). (reported on Endospore 2026-06-21)**
      — after tapping **"I've written it down"**, the recovery-key screen sits unchanged for a few seconds while
      `createRoamUser` runs `createAndManageUser` + `startUserInBackground` + the initial roamed-session setup
      (creating an Android user is genuinely heavy). It DOES advance (CREATE works — the hang was the DIA-20260621-01
      device-owner bug, now fixed), but with no spinner it reads as "frozen." Add a **"Creating your space…"**
      spinner/status on that screen while the create runs (disable the button to avoid double-taps), clearing when
      the new home appears. Chooser-only; affects both editions (the create flow is shared `core/`).
- [x] **Provision robustness: `dpm set-device-owner` can fail at first boot → no kiosk — DONE + VERIFIED on FP3**
      (2026-06-16, DIA-20260616-56). `diaspore_provision.sh` ran `dpm set-device-owner` once and marked itself
      done regardless of result, so a fresh `/data` could boot to a **non-kiosk gate** that never locks. The
      backlog blamed `dpmd` socket timing; the **real** root cause (proven by the full `provision.log` from the
      `su:s0` context) is an **fd-passing SELinux denial**: `dpm`/`pm`/`am`/`settings` are all `cmd`, and `cmd`
      hands ITS stdio fds to `system_server` over the binder transaction. From this `su:s0` init service those
      fds point at the LOG (`diaspore_data_file`, via `>> "$LOG"`) or an `su:s0` fifo (via `$(...)`/pipe), and
      `system_server` is **denied use of both** under enforcing → the whole transaction aborts
      (`Failure calling service device_policy: Failed transaction (2147483646)`) and **the command never runs**.
      Every `cmd` redirected to the LOG/pipe failed; every one left at the init default `/dev/null` worked — and
      the same call from `adb shell` always worked (shell's pty fds are serviceable), which is why a manual
      `adb dpm set-device-owner` always "fixed" it and masked the cause. (The old 180s readiness probe was a
      no-op for the same reason: its `dumpsys account | grep` went through an `su:s0` pipe and never matched.)
      **Fix:** route every `cmd` stdio to `/dev/null` (a `null_device` fd `system_server` CAN use → the call
      executes), and detect success by **reading the state file `system_server` itself wrote** —
      `grep com.diaspore.chooser /data/system/device_owner_2.xml` (a plain `su:s0` file read, no fd hand-off) —
      never by parsing `dpm list-owners`. A simple retry loop replaces the broken probe, and the done-marker is
      written **only** once `owner_ok`, so a genuinely-not-ready boot re-attempts next boot. `ChooserActivity`
      also gained a bounded timer-retry (`lockGate` re-tries every 2s up to ~60s) so the gate kiosks once
      device-owner lands, complementing the existing `onWindowFocusChanged` retry. **Verified:** `fastboot -w`
      wipe → boot with **zero adb** → `[provision] device-owner CONFIRMED after 0 attempts`, `dpm list-owners`
      shows the chooser as DeviceOwner, `mLockTaskModeState=LOCKED`, gate up with Create/Unlock (store conf from
      `/system`). Reusable rule recorded: no `su:s0` Diaspore init script may capture `cmd` output via LOG/pipe.
- [x] **Per-device region / timezone / locale — DONE + VERIFIED on FP3** (2026-06-16, DIA-20260616-58). Was: a
      no-SIM/no-GApps device left `persist.sys.timezone=GMT` (no NITZ, weak geo-tz) → the right instant in the
      **wrong zone** for any non-GMT user; locale stuck at framework `en-US`. Shipped **(1) baked default + (2)
      roam tz/locale with the profile** (the chosen plan; the option-3 gate picker stays a possible later add):
      **(1) Bake** (`diaspore.mk` `PRODUCT_PRODUCT_PROPERTIES`): `persist.sys.timezone=America/New_York` so a fresh
      `/data` comes up in ET not GMT, plus `ro.diaspore.default.{timezone,locale}` as the chooser's fallback
      constants. **(2) Roam** — mirrors the app-list roaming exactly (rides the sealed CE data, no new store ref):
      at logoff `LogoffActivity.capturePrefs()` writes the live tz+locale to its own `files/diaspore-prefs`;
      `diaspore_roamd.sh` surfaces the restored copy to `prefs.out` (like `apps.out`); the agent gains a
      `GET-PREFS` verb (mirrors `GET-APPS`); on login `ChooserActivity.applyRoamedPrefs()` applies them —
      **timezone** via the device owner (`setTimeZone`) and **locale** via the platform `LocalePicker`
      (`updatePersistentConfiguration`) — falling back to the baked default for a profile with none, so each login
      is deterministic. **Two gotchas (both cost a debug, fixed):** (a) `updatePersistentConfiguration` needs
      **`WRITE_SETTINGS`** (not just `CHANGE_CONFIGURATION`) — `SecurityException` otherwise; both are
      signature-granted by the platform cert. (b) `DPM.setTimeZone` is a **silent no-op while auto-detection is
      on** (it's a manual *suggestion* the detector ignores) — so `applyRoamedPrefs` first
      `setAutoTimeZoneEnabled(false)` (tz is profile-managed in the roaming model; `auto_time`/NTP stays on, so the
      instant is still correct). **Verified on FP3:** `fastboot -w` → gate comes up ET (not GMT); created a profile,
      set its zone to America/Los_Angeles, logged off, reset the gate to ET, logged back in → `persist.sys.timezone`
      flipped **ET→LA** (date shows PDT), `GET-PREFS=[tz=America/Los_Angeles|locale=en-US]`, locale apply throws no
      exception, 6/6 apps still roam. NOTE: a *visible* locale (language) flip wasn't exercised — there is no adb
      system-locale setter (`cmd locale` is per-app only) — but locale rides the identical, tz-proven pipeline and
      the apply call succeeds. NOTE: the timezone *resolution* was redesigned after this slice — see
      [`docs/timezone-model.md`](timezone-model.md). DIA-58's baked default + locale roam + prefs plumbing are the
      final foundations; its **timezone** logic (always-disable-auto + capture-live-tz) is an **interim cut**
      superseded by the override / auto-gated priority chain (next item).
- [x] **Timezone resolution: override → NITZ → geo → IP → default (capability-gated)** — design of record in
      [`docs/timezone-model.md`](timezone-model.md). Timezone follows WHERE you are (auto by default, per-profile
      override); locale follows WHO you are (pinned, roams — done in DIA-58). The whole tz policy is **"auto-on
      unless the profile overrides"**: Android's `time_zone_detector` already does NITZ + geo, so we only add the
      profile **override** (manual mode) and an **IP fallback** for when auto is silent. **Capability-gated, not
      device-hardcoded**, so the *same* code covers the SIM-less FP3 (auto silent → baked default) and the
      **FP6 + SIM production target** (NITZ resolves locally, for free).
      **DONE + VERIFIED on FP3 (DIA-20260616-60):** (1) apply rework — `applyRoamedPrefs` gates the auto-disable on
      "override present" (else auto-on + seed baked default), the FP6-ready fix (DIA-58 disabled auto always →
      would suppress NITZ); `capturePrefs` preserves the *explicit* override, not the live tz. (2) **Searchable
      picker** in `LogoffActivity` ("Time zone" → Automatic + all canonical Olson zones w/ offsets + search box).
      (3) **LIVE apply** — picker sends `SET-TZ` to the daemon → `su:s0` worker (`settz`) runs `cmd alarm
      set-timezone` (only root can set the global zone; `/dev/null` stdio per DIA-56), so the pick takes effect in
      the current session, not just next login. Verified: pick Tokyo → JST instantly, Addis Ababa → EAT, Automatic
      → baked default + auto-detection on; override roams across logout→login (ET→Tokyo on re-login).
      **GEO PROVIDER DONE (DIA-20260616-61, verified on FP3):** tier 3 integrated — `com.android.geotz` APEX
      (tz S2 database + system_server jar) + the standalone provider app `OfflineLocationTimeZoneProviderService`
      built into the image, and the diaspore framework-res overlay sets the 3 gates:
      `config_enableGeolocationTimeZoneDetection` (master), `config_primaryLocationTimeZoneProviderPackageName`
      (the provider app), and the real gate `config_enablePrimaryLocationTimeZoneProvider` (defaults false — the
      per-provider opt-in, the thing that kept the provider DISABLED). `is_geo_detection_supported` now `true`,
      provider binds + algorithm RUNs — ⚠️ but this slice only verified "binds + RUNs"; the provider was in fact
      silently crash-looping and never resolved a fix (see DIA-20260617-02).
      **GEO DEFAULT-ON DONE (DIA-20260617-01, verified on FP3):** the per-user `geo_detection_enabled` setting
      defaults OFF (gated by the `system_time` DeviceConfig flag
      `location_time_zone_detection_setting_enabled_default`); `diaspore_provision.sh` flips that GLOBAL flag ON
      at first boot, so every fresh ephemeral roamed user inherits geo-on (no per-user write; no GApps → no server
      sync overwrites). Verified: `fastboot -w` → `device_config get … = true`, `is_geo_detection_enabled = true`
      at the gate with no manual toggle.
      **GEO ACTUALLY RESOLVES DONE (DIA-20260617-02, proven on FP3):** the fix that makes tier-3 geo *work*. The
      standalone provider is a priv-app, but the 3 location perms are **dangerous (runtime)** perms that
      `privapp-permissions` does NOT grant → `SecurityException` at `getCurrentLocation()` → ~9 boot crashes →
      ActivityManager ~4h restart backoff → provider never ran (so DIA-61/01's "supported/enabled/RUNNING" was
      true but it stayed INITIALIZING→UNCERTAIN forever; this also masked GNSS as "never engaging"). Fix: a
      **default-permissions** exception (`vendor/diaspore/default-permissions/diaspore-geotz-location.xml`) grants
      `ACCESS_FINE/COARSE/BACKGROUND_LOCATION` at **every user's creation** (gate user 0 + ephemeral roamed users
      — provider binds per-current-user via `ServiceWatcher`, so a user-0 `pm grant` would miss roamed sessions).
      Proven on FP3 (clean boot, no manual grant): provider healthy, `getCurrentLocation(FUSED,HIGH_ACCURACY)`
      propagates to GPS, mock **Tokyo→`Asia/Tokyo`** / **London→`Europe/London`**, state **CERTAIN**. So Automatic
      now genuinely uses geo when a fix lands.
      **IP FALLBACK DONE (DIA-20260617-03, tier 4):** when a profile has no override, `applyRoamedPrefs` seeds
      AUTOMATIC from a coarse public-IP zone (chooser → daemon `RESOLVE-IP-TZ` → agent does the egress, one
      HTTPS GET to `ipinfo.io/json`; validated against known zone IDs), else the baked default. Applied with
      auto-detection still ON, so NITZ/geo override it — the IP zone is only a fast better-than-baked first guess.
      Best-effort by design (VPN-fragile + leaks coarse location), so the manual override stays the dependable
      anchor for our (VPN/indoor/SIM-less) audience. **The full chain is now complete.**

## Security hardening (cross-cutting)
- [x] **SELinux enforcing** for the agent + init services (P4.2c confined `diaspore` domain; `inet`-group, not net_raw).
- [x] **Screen-lock / unattended-session protection ([lock-model.md](lock-model.md)) — DONE + verified on FP3.** Mid-session the keyguard is
      OFF (boot design → the gate is the sole auth), so a locked phone wakes straight to home AND `/data` stays
      decrypted at rest. Fix (Chester 2026-06-19, DIA-20260619-16) = **re-auth to resume + auto-logoff backstop**:
      a session-scoped keyguard credential set on login / cleared on logoff (FBE-locks `/data` on screen-off, no
      boot flash), with an idle-timeout + failed-attempt backstop that seals+wipes back to the gate. Strongest on
      Endospore (secure-element-throttled credential). **DONE + verified on FP3 (2026-06-23, L1–L5; PRs
      #120/#122/#124):** session-scoped credential set on the first screen-off (login lands on home) and
      destroyed by the ephemeral-user reap on logoff; warm resume; idle-timeout + failed-attempt auto-logoff;
      per-profile idle (`idle=<min>` roams). **L3 correction:** screen-off does NOT FBE-lock CE (a locked-but-
      running user stays `RUNNING_UNLOCKED`), so the keyguard is a UI re-auth gate and the **auto-logoff is the
      real at-rest guarantee**. Endospore SE-throttled short-PIN = #18 (separate). **PIN/pattern PARKED (Chester,
      2026-06-30): credential = passphrase on both editions** — a short secret can't roam safely without a server
      oracle (which would hold a per-profile unlock handle = a profile list, eroding blind login) or a user-carried
      token, and strict boot-wipe makes tap-to-resume-after-reboot moot; so #55 (roaming PIN) + #18 (Endospore SE
      PIN) are deferred-by-decision. See [resumable-session.md](resumable-session.md) "Decisions (resolved)".
- [x] **Phase-1 security audit + fixes — DONE (2026-06-18, DIA-20260618-08).** Defensive review of the trust
      boundaries (daemon socket protocol, su:s0 workers, tar/restore from the untrusted store, OTA authenticity,
      sepolicy, creds-at-rest). Three findings, all fixed + proven on FP3: **(#1 HIGH) tar-slip → root arbitrary
      write** — `untarFrom` had no containment check on `hdr.Name`/`it.Name` and runs as root via `diaspore_roamd`
      (`/data/user/N`) and `diaspore_otad` (`/data/ota_package`); the OS payload is sealed under a **public** pass,
      so a hostile store could forge a `../` entry → root write *before* update_engine's signature check. Fixed
      with a `safeJoin` containment guard (+ unit tests). **(#2 def-in-depth) socket peer auth** — the `0666`
      socket relied solely on SELinux's `connectto` (chooser-only); added an `SO_PEERCRED` trust-on-first-use pin
      (no privileged read/cap/sepolicy; a packages.list approach was dropped as it needed `CAP_DAC_*`). **(#3 low)
      `diaspore_roamd.sh` now numeric-validates `$uid`** before the root `USERDIR` path. **Sound as-is:** OTA boot
      authenticity (update_engine verifies the payload signature vs the baked custom key); no command injection;
      blind-login + head signing + enrollment rate-limit. Creds-at-rest: name+pass live plaintext only in tmpfs
      (RAM, root 0700, power-off-wiped) — acceptable; baked `/system` store creds stay the known caveat (→ broker).
- [~] Least-privilege / scoped store credentials (or per-session creds via the discovery layer) — avoid baking
      long-lived keys into the image. **Keyless bootstrap DONE (2026-06-15, DIA-20260615-44):** discovery LOOKUP is
      now **anonymous public-read** — with `DISCO_ACCESS_KEY/SECRET` blank + a public-read discovery bucket, the
      daemon GETs the sealed config over a plain credential-free HTTP GET (`discoGetAnon`), so a **fresh device bakes
      NO creds at all** to bootstrap (safe: discovery holds only sealed, zero-knowledge blobs at unguessable refs).
      `discoCanLookup()` = endpoint+bucket; `discoConfigured()` (full creds) still gates **publish** only. Unit-tested
      (`TestDiscoAnonLookup`: anon GET + unseal + blind miss). REMAINING: scoped **WRITE** tokens for publish (the
      enrol/login auto-publish still uses account creds on devices that have the store) → the Phase-2 **token broker**;
      a managed least-knowledge gateway; *(Filebase key rotated 2026-06-14, new key proven; OLD key still pending
      revocation by Chester.)*
