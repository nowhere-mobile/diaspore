# Diaspore — App model

How apps work when the OS is stored locally and user state roams. Companion to
[design.md](design.md) and [roadmap.md](roadmap.md). Decided 2026-06-09.

## The three-way split

A naive "all of `/data` roams" is wrong — `/data` holds three different kinds of thing,
and only one is the secret:

| Slice of `/data` | Examples | Sensitivity | Treatment |
|---|---|---|---|
| **Private data** | `/data/user/<id>/<pkg>`, accounts, settings, keystore, media/files | **the secret** | encrypted roaming; **wiped on power-off** |
| **App code** | `/data/app/<pkg>` APKs + native libs | **public binaries** | per-device: **re-download from distributor**, cache locally |
| **Regenerable** | dexopt (`oat/`, `dalvik-cache`), app caches | derived from public inputs | regenerate, or keep in a non-secret local cache |

Only the private slice must be encrypted, roamed, and wiped. App code is public → treat it
like the OS. Regenerable bytes are never roamed as a secret.

## App code: metadata + re-download (not carry)

Roam a small **app manifest** per profile — `{ package, source, version }` — **not** the APK
bytes. On a new device, reconstitute each app by re-downloading its code from its distributor:

- **Open apps (F-Droid, etc.):** content-addressable, trivial.
- **Play apps:** re-download via **Aurora Store** or **microG** (FOSS, no full Google Play
  Services — fits the de-Googled / unlocked Diaspore OS). Play's App Bundle delivery also gives
  **device-correct splits** (right ABI/density) — better than carrying phone 1's APK to phone 2.
- **Sideloaded apps with no distributor:** fall back to carrying / content-addressing the APK
  in the store.

Benefits: tiny roaming payload (a list, not gigabytes); device-correct, up-to-date code;
license-clean (you don't redistribute APKs).

## Restore ordering = install-then-restore-data

Android's PackageManager treats a package that's in the registry (`packages.xml`) but whose APK
is missing as **orphaned, and purges it (and its data)**. So restore must be, per app:

1. (re)install the app **code** (Aurora / F-Droid / carried),
2. **then** attach its restored private **data**.

This mirrors Android's Auto Backup (data restore is keyed to app install). Never dump app data
without the code present, or a phone-switch silently deletes it.

## dexopt / ART

Compiled code (`oat`/`art`) is derived from the public APK + OS — don't roam it as a secret.
Either regenerate on first run (slower first boot) or keep it in a **non-secret local cache**.
It's portable across devices only when the OS/ART/arch match — which they do, since Diaspore
keeps every device on one OS version via content-addressed OTA.

## Privacy boundary

The app **list** profiles you → **private** → part of the encrypted roaming state.
The app **code** is **public** → fetched from public distributors, cached locally.

## Dependencies & caveats

- **First boot on a new device needs network + a store client** (Aurora / F-Droid) to
  re-download missing code. Offline → those apps are **deferred**: their data is held and
  re-attaches when the code arrives. **No data loss.**
- **Version drift** — the distributor serves the current version; usually fine.
- **Play Integrity apps** (some banking) won't run on an unlocked bootloader — an accepted
  Diaspore tradeoff, independent of this model.
