# Diaspore — Timezone & locale resolution (design of record)

Status: design of record · 2026-06-16 · pairs with [roaming-boundaries.md](roaming-boundaries.md),
[identity-model.md](identity-model.md), [roadmap.md](roadmap.md)

How Diaspore decides the system **timezone** and **locale** across the roaming model and across hardware —
the SIM-less dev **FP3** and the SIM-equipped **FP6** production target (roadmap Phase 4). Agreed in a
design discussion after DIA-20260616-58. DIA-58 shipped the foundations (baked default + locale roam + the
prefs plumbing, all proven on FP3); its **interim** timezone logic is superseded by the priority chain
below.

## The split: timezone follows WHERE you are; locale follows WHO you are

Two questions that deserve opposite answers:

- **Timezone = location.** A roaming/travel device should show *local* time wherever you sign in — land in
  Tokyo, get Tokyo time. So timezone is **auto-detected by default**, with an optional per-profile override.
- **Locale = identity.** Your language is yours; you do NOT want the phone in Japanese just because you are
  in Tokyo. So locale is **pinned to the profile and roams** — no auto-detection.

This asymmetry is the core decision; the rest follows from it.

## Timezone resolution order (capability-gated)

Resolve in this priority, skipping any source the device/runtime can't provide:

1. **Profile override** — an explicit Olson ID stored on the profile (e.g. `America/New_York`). Manual
   mode; always wins. The dependable anchor.
2. **Telephony NITZ** — the zone the modem learns from the cell network at registration. Instant,
   on-device, **no extra leak** (the carrier already knows your tower). Needs a SIM + a carrier that
   actually broadcasts NITZ (US carriers / MVNOs / roaming are spotty).
3. **Geolocation — AOSP offline location tz provider** — a GPS fix resolved against a bundled on-device tz
   database. **Fully offline, nothing leaves the device, no GApps** — the most privacy-aligned auto source.
   Accurate, but a fix is slow / unreliable indoors. **INTEGRATED in DIA-20260616-61** (`is_geo_detection_supported`
   now `true` on the FP3): the `com.android.geotz` APEX (the tz S2 database + the system_server classpath jar)
   + the standalone provider app `OfflineLocationTimeZoneProviderService` are built into the image, and three
   framework-res config gates are set in the diaspore overlay — `config_enableGeolocationTimeZoneDetection`
   (master, already true), `config_primaryLocationTimeZoneProviderPackageName` (was empty → the provider app),
   and the real gate **`config_enablePrimaryLocationTimeZoneProvider`** (defaults false — the per-provider
   opt-in). With detection on, the provider binds and the algorithm RUNs (awaiting a fix).
4. **Wi-Fi / IP** — derive a coarse zone from the public IP over the network the device already has.
   Instant and always-on with internet, but **coarse** and **wrong behind a VPN** (returns the exit node's
   zone). A Diaspore-built fallback, *not* a native Android source.
5. **Baked default** — the per-market `persist.sys.timezone` baked in the build (`America/New_York` today).
   The pre-login gate and any "nothing resolved" case land here.

### Lean on Android's own detector for tiers 2 + 3

Android's `time_zone_detector` ALREADY implements NITZ (2) and geolocation (3) with its own internal
priority, so Diaspore does **not** hand-roll those tiers. The entire timezone policy is one rule:

- **Override present** → force MANUAL mode (`setAutoTimeZoneEnabled(false)`) + `setTimeZone(override)`.
- **No override** → AUTO mode ON (`setAutoTimeZoneEnabled(true)`); Android picks NITZ/geo if available. If
  auto stays silent (no NITZ signal, no geo provider), apply the **IP fallback** (4); else the **baked
  default** (5).

That "**auto-on unless the profile overrides**" rule is **capability-gated, not device-hardcoded** — the
same code adapts to each device:

- **FP3 (dev, no SIM, no geo provider):** auto is silent → IP fallback → baked default. An override pins.
- **FP6 (production, SIM):** auto on → **NITZ resolves locally with zero Diaspore-specific code**. An
  override pins.

So FP6 is "covered" by *designing it in* now: the NITZ tier lights up for free the moment a SIM is present,
because Android owns it. The only genuinely-later items are the IP fallback and (optionally) adding the
offline geo provider.

> **Reworked in DIA-20260616-60** (was interim in DIA-58, which disabled auto-detection *always* and captured
> the *live* tz — correct only for "SIM-less FP3 with no override UI", and on FP6 it would have suppressed
> NITZ). Now `applyRoamedPrefs` gates the auto-disable on "override present" (else auto-on, seeding the baked
> default as the floor), and `capturePrefs` preserves the *explicit* override instead of pinning the live tz.

### Why not just IP everywhere?

IP is the easiest source but the worst fit for the Diaspore audience: it **exfiltrates coarse location** to
whoever resolves it, and it **lies behind a VPN** — and Diaspore users are exactly the VPN/privacy crowd,
often indoors, sometimes SIM-less. So IP is a best-effort *convenience*, never the source of truth. The
offline geo provider is the privacy-preferred auto source long-term (offline, on-device, no leak) despite
the GPS latency; the manual override is the anchor for everyone the auto-chain fails.

## How an override gets set — the picker (DIA-20260616-60, built + proven on FP3)

A **searchable timezone picker** in the session's logoff/account screen (`LogoffActivity` → "Time zone";
"Automatic — use your location" + all canonical Olson zones with their GMT offset, filtered by a search
box). Picking a zone does **two** things:
- **Persists** the chosen Olson ID (empty = Automatic) into the profile prefs (`files/diaspore-prefs`), so
  it roams and `applyRoamedPrefs` re-applies it on every login.
- **Applies it LIVE.** A logged-in *secondary* user can't set the global zone, and the confined daemon
  can't either — only **root** can — so the picker sends `SET-TZ <olson>` to the daemon, which drives the
  `su:s0` worker (`diaspore_roamd.sh` `settz`) to run `cmd alarm set-timezone` (override: auto off + pin;
  Automatic: seed the baked default + auto on). `cmd` needs `/dev/null` stdio from `su:s0` (the cmd
  fd-passing denial, [[diaspore-suzero-cmd-fd-denial]] / DIA-56). Verified on FP3: picking Tokyo flips the
  session clock to JST instantly; Addis Ababa → EAT; Automatic → baked default + auto-detection on.

## Locale

Roams with the profile, pinned to identity (DIA-58, as-built + proven on FP3): captured at logoff, applied
at login via the platform `LocalePicker` (`updatePersistentConfiguration` — needs **`WRITE_SETTINGS`** +
`CHANGE_CONFIGURATION`, both platform-cert granted). Falls back to the baked `ro.diaspore.default.locale`
(`en-US`). No auto-detection.

## Plumbing (shared with the app-list roam, DIA-58)

The timezone override and the locale ride the **sealed CE data** with no new store ref: the chooser writes
them into its own `files/diaspore-prefs` (sealed with `/data/user/N`), `diaspore_roamd.sh` surfaces the
restored copy to `prefs.out`, the agent's `GET-PREFS` verb hands it back, and
`ChooserActivity.applyRoamedPrefs()` applies them on login. Mirrors the app-list
(`diaspore-apps.list` / `apps.out` / `GET-APPS`) exactly.

## Status

- **DONE** (DIA-20260616-58, proven on FP3): baked default (tz + locale), locale roam, the prefs plumbing.
- **DONE** (DIA-20260616-60, proven on FP3): the override / auto-gated apply rule (FP6-ready — auto-on so
  NITZ works on a SIM device); the searchable timezone picker; **live** apply via the `su:s0` worker; the
  override roams across logout/login.
- **DONE** (DIA-20260616-61, FP3): the **AOSP offline geo provider** (tier 3) is integrated —
  `is_geo_detection_supported=true`, the provider binds + the algorithm RUNs when detection is on. ⚠️ This
  slice *appeared* to work (supported/enabled true, algorithm RUNNING) but never actually resolved a fix —
  the provider was silently **crash-looping** (see DIA-20260617-02). It only verified "binds + RUNs", not an
  end-to-end resolution.
- **DONE** (DIA-20260617-01, verified on FP3): **Automatic uses geo by default**. The per-user "use location
  for time zone" setting defaults OFF (gated by the `system_time` DeviceConfig flag
  `location_time_zone_detection_setting_enabled_default`); `diaspore_provision.sh` flips that **global** flag
  ON at first boot (so every fresh ephemeral roamed user inherits geo-on, no per-user write; no GApps → no
  server sync overwrites it). Verified: `fastboot -w` → `device_config get … = true`, `is_geo_detection_enabled
  = true` at the gate with no manual toggle.
- **DONE** (DIA-20260617-02, proven on FP3): **geo actually resolves a fix → tz**. Root cause the prior two
  slices missed: the standalone provider app is a *privileged* app, but `ACCESS_FINE/COARSE/BACKGROUND_LOCATION`
  are **dangerous (runtime)** permissions — and `privapp-permissions.xml` only grants signature|privileged
  perms, never runtime perms. So the provider threw `SecurityException` the instant it called
  `getCurrentLocation()`; at boot it crashed ~9× in seconds, ActivityManager backed its service restart off to
  ~4h, and it never ran again — leaving `supported/enabled=true` but the algorithm stuck INITIALIZING→UNCERTAIN
  forever (which also *masked* the GNSS engine as "never engaging": no live request was ever made). Fix: a
  **default-permissions exception** (`vendor/diaspore/default-permissions/diaspore-geotz-location.xml`) grants
  the three location perms to the package at **every user's creation** — the gate (user 0) AND every ephemeral
  roamed user, since the provider binds **per-current-user** (`ServiceWatcher`, `serviceIsMultiuser=false`), so
  a user-0-only `pm grant` would miss roamed sessions. Proven on FP3 (clean boot, no manual grant): the
  provider comes up healthy (no crash), `getCurrentLocation(FUSED, HIGH_ACCURACY)` **propagates to the GPS
  provider**, and an injected location resolves to its Olson zone — mock **Tokyo → `Asia/Tokyo`**, **London →
  `Europe/London`**, state **CERTAIN** with a real `GeolocationTimeZoneSuggestion`. A real outdoor GPS fix is
  the same path (the indoor blocker is only the satellite fix, not the integration).
- **DONE** (DIA-20260617-03, FP3): the **IP fallback** (tier 4). When a profile has **no override**, the
  chooser's `applyRoamedPrefs` picks the AUTOMATIC *seed* by asking the daemon (verb `RESOLVE-IP-TZ`) for a
  coarse zone derived from the device's **public IP** — the **agent** does the egress (one HTTPS GET to a
  no-auth IP-geo service, `https://ipinfo.io/json`, parsing the top-level `timezone`), since it's the single
  component already allowed out under sepolicy (a future least-knowledge gateway can proxy it). (ipinfo was
  chosen over ipapi.co, which `429`s the default Go user-agent — we don't spoof a browser UA.) A valid zone becomes the seed (validated
  against the platform's known IDs); otherwise the **baked default** (tier 5) is used. Crucially the seed is
  applied with **auto-detection still ON** (`setTimeZone(seed)` then `setAutoTimeZoneEnabled(true)`), so a real
  **NITZ/geo fix always overrides it** — the IP zone is just a fast, better-than-baked first guess. Best-effort
  by design: VPN-fragile (returns the exit node's zone) and leaks the public IP to the resolver, so it never
  wins over an actual fix and the manual override stays the anchor. So the full chain (override → NITZ → geo →
  IP → baked default) is now complete.
