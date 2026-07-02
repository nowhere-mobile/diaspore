# Export your data (port-out) — design of record

**Status:** design. **Work ID:** DIA-20260628-09. **Scope:** the Profile app's *Your data* screen, a
device-side export service, and the formats it emits. The **export counterpart** of
[move-to-nowhere.md](move-to-nowhere.md) (import); the concrete build for the "Export my data" feature
sketched in [data-portability.md](data-portability.md) §ii.

## Why

People should be able to **leave** with their data — that's the whole anti-lock-in story. Nowhere's model
makes this *easy by design*: your session is a real Android user with your data roamed in, you hold the key
(name + passphrase), and the store holds only ciphertext you can self-host. So "port out" isn't prying data
loose — it's **packaging what's already in your hands** into portable, standard files you can take anywhere.

This doc covers two distinct things the *Your data* screen offers, which people conflate:

1. **Back up now (durability)** — *built (P1, DIA-20260628-09)*. Force an immediate seal of the live session
   to your store, capturing your latest changes on demand (the data already roams ~every minute). This keeps
   your data **safe**; it does not get it **out**. Reuses the periodic-seal path (`BACKUP` → the daemon's
   `triggerRoamWorker "out"`), with the logoff progress channel for a "Backing up… N%" line.
2. **Export a copy (portability)** — *this design*. Write your data out as **standard files** to a
   destination you control (USB/SD, a pullable folder, or a portable archive), so moving to another phone —
   nowhere or not — is turnkey instead of manual.

## Principles (invariants)

- **Standards-first.** Portable formats only (vCard, iCal, files, app-native 2FA/notes exports). No
  proprietary scraping — the same honesty as import.
- **You already hold the key.** Export decrypts in your live session (you're logged in); nowhere is not
  involved and learns nothing. Unlike import, there's no crypto to do — the data is right there.
- **App-by-app, via each app's own export path.** Mirror the import matrix in reverse: each data type comes
  out of the open app that owns it (Contacts → vCard, etc.).
- **Plaintext lands on *your* chosen destination.** An exported copy is **not** sealed — it's portable
  precisely because it's plain files. The UI must say so (you're taking your data out of the encrypted vault).
- **Honest about gaps.** App-sandboxed proprietary data that has no export path doesn't come out — stated
  plainly, same as import's walled-garden note.

## Export matrix (mirror of the import matrix)

| Data | nowhere source app | Export format | Cleanliness |
|---|---|---|---|
| Contacts | Contacts/Dialer (ContactsContract) | vCard `.vcf` | clean |
| Calendar | Calendar (CalendarContract) | iCal `.ics` | clean |
| Messages (SMS) | Telephony.Sms provider | XML (SMS Backup & Restore shape) | clean (MMS not yet) |
| Call log | CallLog provider | XML | clean |
| Photos / files / docs | media dirs | copy files out | clean |
| 2FA seeds | Aegis | Aegis export (encrypted or plain JSON) | clean |
| Notes | Notally | text/markdown / JSON export | clean |
| Bookmarks | Fennec | HTML / Firefox export | clean |
| Loyalty cards | Catima | Catima export | clean |
| **App-sandboxed proprietary data** | — | — | **does not export** |

## Architecture

The session is a real Android user, so most export is **standard Android export run on demand**. Two layers,
matching import's split (tool app-format-agnostic; per-app knowledge confined to a service):

1. **Export bundle (standard files).** A privileged, **one-shot** device-side export service walks each
   app's own export path — ContactsContract → `contacts.vcf`, CalendarContract → `calendar.ics`, media copy,
   Aegis/Notally/Catima via their export intents — into a versioned bundle dir (`contacts.vcf`,
   `calendar.ics`, `media/`, `twofa.json`, `notes/`…). Same bundle shape as the import bundle, so a nowhere →
   nowhere move is bundle-symmetric.
2. **Destination — sealed to your store, fetched from the web (DECIDED 2026-06-28).** A roamed session is a
   *secondary Android user*, and that breaks the obvious "write to Downloads, pull over USB" plan: its
   MediaStore primary volume isn't even registered, and **USB/MTP exposes user 0, not the roamed user**
   (proven on FP3 — `MediaStore` throws *Volume external_primary not found*, and `/data/media/<uid>` is an
   empty, unindexed dir). So the export bundle is instead **sealed client-side under your profile key and
   written to your store** at a dedicated `export/<name>` ref, then **downloaded + decrypted from the web
   storefront** with your name + passphrase. This is the zero-knowledge path and the most nowhere-native: the
   store holds only ciphertext; only you, in the browser, hold the key. (USB-OTG to a plugged-in drive stays a
   possible *on-device* alternative for a later phase, but the store + web route is the default.)

```
 live nowhere session (logged in)      chooser builds, daemon seals           retrieval (web storefront)
   ContactsContract -> contacts.vcf ->  bundle sealed under your profile  ->   sign in (name + passphrase),
   CalendarContract -> calendar.ics     key, stored as export/<name>           browser derives the key, fetches
   (chooser holds the read perms)       (the store learns nothing)             export/<name>, decrypts, and
                                                                               downloads contacts.vcf / .ics
```

### Cold-vault export (separate, free today)

Distinct from per-app export: because the **store is only ciphertext**, you can already snapshot the whole
bucket to anywhere (Glacier, a drive, a NAS) and restore by re-pointing + logging in — no code, no fee
([data-portability.md](data-portability.md) §iii). A future "Export vault" button just wraps that
`rclone`-style copy into one tap. This is the *encrypted* archive path; the per-app export above is the
*portable plaintext* path. They serve different needs (own-your-ciphertext vs. take-it-to-another-product).

## Phasing

- **P1 — Back up now.** *Done* (DIA-20260628-09): on-demand seal + the *Your data* screen.
- **P2a — read + build (done).** *Contacts (vCard) + calendar (iCal)* (DIA-20260628-09): "Export a copy" reads
  the providers under an on-demand `READ_CONTACTS`/`READ_CALENDAR` grant (not standing-granted to the kiosk;
  contacts use the provider's own `CONTENT_MULTI_VCARD_URI`) and builds standard files. The first cut wrote to
  `Downloads` via MediaStore — **abandoned**: roamed = secondary user, no MediaStore volume / no MTP (see
  Destination).
- **P2b — seal the bundle to the store (device).** *Done + proven on FP3* (DIA-20260628-09): chooser zips the
  built vCard/iCal + a `manifest.json` and hands the bundle to the daemon's length-prefixed `EXPORT` command;
  the daemon seals it under the session's profile key (`resolveDK` → `seal`) and writes `ref/export/<name>`
  (`postBlob` + `putRef`). Round-trip verified: the native agent's new `export-fetch <store> <name> <pass>
  <destZip>` re-derived the key, fetched + decrypted `export/a`, and recovered the exact zip (vCard with the
  test contact + manifest). The store held only ciphertext. `export-fetch` is also the reference retrieval P3
  mirrors.
- **P3 — storefront retrieval (nowhere-cloud).** A web page: sign in (name + passphrase) → derive the key in
  the browser → fetch + decrypt `export/<name>` → download `contacts.vcf` / `calendar.ics`. The user-facing
  half; parallels the WebUSB installer's browser-crypto approach.
- **P4a — messages + call log.** *Done* (DIA-20260628-12): SMS (`Telephony.Sms` → XML, SMS-Backup-&-Restore
  shape) + call log (`CallLog` → XML), folded into the same `export/<name>` bundle via the SAME provider-query +
  on-demand-grant pattern as contacts/calendar (best-effort: whatever you grant gets exported). No daemon or
  storefront change — the bundle is just a zip the web download hands back. MMS (multipart) deferred.
- **P4b — media.** Bulky plaintext, so it does NOT fit the sealed-bundle/browser-download model — the path is
  **USB-OTG** (write to a plugged-in drive), a separate hardware mechanism. (For nowhere→nowhere, media already
  roams; export-media is mainly for *leaving* nowhere.)
- **P4c — app-native exports.** Aegis / Notally / Catima / Fennec are sandboxed and mostly don't expose a callable
  export, so this is root-mediated per-app work (or guiding the user to each app's own export) — one app at a time.
- **P5 — Export vault** (cold ciphertext archive, one tap over the BYO-store path; [data-portability.md](data-portability.md) §iii).

## Security / privacy

- **Sealed under your key end-to-end** — the bundle is sealed client-side on the device and only ever decrypted
  in *your* browser at retrieval; the store and the gateway see only ciphertext. Plaintext exists just at the
  two endpoints you control (the live session and your browser), never in transit or at rest server-side.
- **No proprietary scraping** — only each app's sanctioned export path; gaps are documented, not worked
  around.
- **Throwaway profiles** have no store, so the store + web route can't carry their export — a throwaway that
  wants out needs the on-device/USB-OTG path (P4). Surface this rather than silently dropping it.

## Open decisions

1. **Default destination** — *resolved 2026-06-28: seal to the store + fetch from the web* (roamed = secondary
   user, so Downloads/MTP is out). USB-OTG remains a later on-device alternative.
2. **Bundle format** — a small tar of `contacts.vcf` + `calendar.ics` (+ a `manifest.json` with version/count)
   sealed as one blob at `export/<name>`, vs. one sealed ref per file. (Leaning: one tar bundle.)
3. **Which apps in tier-1** beyond contacts/calendar/media (proposed: none — keep tier-1 to the framework
   providers + media; app-native exports are P4).
4. **Bundle versioning** shared with the importer (one schema, both directions).
5. Whether the export service rides the existing su:s0 roam worker or a dedicated one-shot component.

## Verification

- **P1:** "Back up now" on a live session reports DONE; confirm the store footprint reflects new changes
  (GET-USAGE), and that pulling the plug right after loses nothing.
- **P2+:** export contacts/calendar/media to the destination, import them on a *stock* phone (proving the
  files are standard), and on a fresh nowhere phone (proving bundle symmetry with the importer).
