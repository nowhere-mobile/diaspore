# Move to nowhere — switcher data migration (design of record)

**Status:** design. **Work ID:** DIA-20260628-06. **Scope:** the agent (seed/import), a one-time
device-side import service, and an onboarding tool. Sibling to the web installer
([nowhere-cloud `docs/installer.md`]); the portability counterpart of [data-portability.md](data-portability.md).

## Why

People switching from iOS / stock Android arrive with data — contacts, calendar, photos, 2FA,
notes — they want to bring. nowhere is **not a device clone**: the OS is different *and* the app
set is intentionally open/F-Droid (Fennec, Conversations, Aegis, OrganicMaps, Notally, Catima…),
not the proprietary apps they're leaving. So migration is **standards-based, app-by-app import**
into nowhere's open apps, **seeded into the encrypted roaming store** so the *first login* on a
nowhere phone is already populated. This doc designs the "Move to nowhere" tool + the device-side
import that makes the clean-porting essentials feel like one upload.

## Principles (invariants)

- **Zero-knowledge.** The import is sealed **client-side** under the passphrase-derived key before
  anything reaches the store — nowhere never sees plaintext or the key. The tool reuses the
  **open** agent's seal (`core/agent`), so the privacy claim stays auditable.
- **Standards-first.** Portable formats only (vCard, iCal, files, 2FA exports). No proprietary
  scraping, no surveillance-shaped backdoors.
- **App-by-app into the open set.** Each data type maps to a specific nowhere app's import path.
- **One-time seed, then it roams.** After import the data lives in the store and roams like
  everything else; the tool is only the *seeding* path.
- **Honest about gaps.** iMessage / WhatsApp history and proprietary app data **don't** port —
  walled-garden lock-in, surfaced plainly and framed as the point of leaving.

## Migration matrix

| Data | Source export | nowhere target | Cleanliness |
|---|---|---|---|
| Contacts | vCard `.vcf` | Contacts/Dialer (ContactsContract) | clean |
| Calendar | iCal `.ics` | Calendar (CalendarContract) | clean |
| Photos / files / docs | files | media dirs → roam | clean |
| Bookmarks / history | Firefox sync / HTML | Fennec | clean |
| 2FA seeds | Aegis / andOTP / Authenticator export | **Aegis** (native import) | clean |
| Passwords | iOS Keychain / Google PM → CSV | KeePass-style manager *(TBD which ships)* | clean-ish |
| Notes | text/markdown export | Notally | clean |
| Loyalty cards | Catima import / barcodes | Catima | clean |
| **iMessage / WhatsApp history** | — (no clean export; tied to Apple / phone-number + a different protocol — nowhere msg is XMPP) | — | **does not port** |
| **Proprietary app data / health** | — | — | **does not port** |

## Architecture

The crux: **seed an encrypted profile vault with the switcher's data before first login**, so
login restores it. The agent already has the pieces — `create-vault` (identity + recovery code),
`push` (seal a dir → store as CDC chunks), `restore`, and the per-user roam model where each app's
`/data/user/N/<pkg>` roams. What's missing is turning *standard export files* into *app-specific
state*.

We **don't** pre-build each app's private DB (brittle, per-app-format coupling). Instead, a hybrid:

1. **Migration bundle (standard files).** The tool packages the switcher's exports into a
   versioned dir of portable files — `contacts.vcf`, `calendar.ics`, `media/`, `twofa.json`,
   `bookmarks.html`, `notes/`… — and seals it into the vault under `import/`.
2. **Device-side import service (one-time, first login).** After login restores the (default) app
   set, a privileged one-time importer detects `import/` and runs each **app's own import path** —
   contacts via ContactsContract, calendar via CalendarContract, media copy into the gallery dirs,
   Aegis via its import intent, etc. — then **clears the bundle**. From then on, the app data roams
   normally.

This keeps the *tool* app-format-agnostic (it only ever handles standard files) and confines
app-specific knowledge to the device-side importers, which can grow incrementally.

```
 switcher's phone           "Move to nowhere" tool (client-side)        nowhere phone (first login)
   export vCard/iCal/  ->   create identity (vault + recovery)     ->   restore profile + default apps
   photos/2FA/...           seal migration bundle (import/)             import service runs per-app
                            upload ciphertext to the store              imports, then clears import/
                            (zero-knowledge; nowhere sees only          -> contacts/calendar/photos/
                             encrypted chunks)                              2FA already there
```

### The tool — two flavors

- **Desktop (recommended first):** runs the **native** agent, seals locally, uploads ciphertext.
  Simplest — the native seal is proven (`core/agent`).
- **Web (later):** browser wizard, the agent compiled to **WASM** to seal client-side (parallels
  the WebUSB installer's browser-first approach). More work; defer.

## Phasing

- **P1 — manual path (works today).** Document export → transfer → import using the apps' existing
  import UIs. No new code; validates which apps import what + the realistic gaps. The honest MVP.
- **P2 — migration bundle + device-side import service.** Define the versioned bundle format and a
  first-login, one-time importer (start with the **tier-1** importers: contacts, calendar, photos,
  2FA). This is the core enabler.
- **P3 — seeding tool (desktop).** The wizard: create identity → seal the bundle into the store →
  "flash a nowhere phone + sign in."
- **P4 — web wizard (WASM agent).** Browser version of P3.
- Per-app importers grow over time beyond tier-1 (notes, bookmarks, loyalty, passwords).

## Security / privacy

- **Client-side seal only** — the open agent's seal; nowhere holds ciphertext + sizes, never the
  key or plaintext (same guarantee as normal roaming).
- The migration bundle is **encrypted at rest** in the store like all profile data.
- The device-side importer is **one-time** and **clears `import/`** after running — no lingering
  plaintext import dir in the roamed state.
- **Gaps are documented, not worked around** — no proprietary-format scraping that would
  compromise the privacy posture or the user's source-account security.

## Open decisions

1. **Tier-1 importer set** to build first (proposed: contacts, calendar, photos, 2FA).
2. **Desktop vs web** for the first tool (recommend desktop / native agent).
3. **Password manager** that ships (e.g. KeePassDX) — fixes the password import target.
4. **Bundle format + versioning** (so old bundles import on newer OSes).
5. Whether the import service runs as part of the existing roam worker (su:s0) or a dedicated
   one-time component.

## Verification

- **P1:** manually import a vCard / iCal / 2FA export into the apps on a device, log off + back on,
  confirm it **roamed** (survives the amnesiac wipe via the store).
- **P2:** seal a hand-built bundle into a test vault, log in on a fresh device, confirm the importer
  populates contacts/calendar/photos/2FA and then clears `import/`.
- **P3/P4:** end-to-end — switcher exports → tool seals → first login on a nowhere phone shows the
  imported data; verify nowhere only ever stored ciphertext.

## Framing for switchers

Three tiers, stated up front: **bring your essentials** (contacts/calendar/photos/2FA — clean),
**rebuild your apps** (open equivalents, fresh state), **leave the lock-in behind** (iMessage etc.
— by design). The "Move to nowhere" tool is what makes tier 1 feel like magic, and is the natural
onboarding artifact after the installer.

[nowhere-cloud `docs/installer.md`]: ../../nowhere-cloud/docs/installer.md
