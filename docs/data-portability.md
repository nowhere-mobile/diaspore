# Nowhere — User-data portability (move in, move out, cold backup)

Status: design · 2026-06-23 · companion to [portability.md](portability.md) (which covers *device/image*
portability — how many OS images). This doc covers a user's **data**: getting it **in** from another phone,
**out** to another phone, and **archived** to cold/self-hosted storage with no service fee.

## Why portability here is different (and mostly a strength)

Nowhere's data model is unusual, which changes what "portability" means:

- An identity is **blind-login**: the `(name, passphrase)` pair *is* the key. There is no account record to
  migrate — the identity is derived, not stored.
- Your data lives in a **client-encrypted vault** in an S3-compatible store. The store holds **only
  ciphertext** (content-addressed CDC chunks) — the least-knowledge / zero-knowledge model.
- A session is an **ephemeral** Android user that the vault **roams into** on login and that is
  **crypto-shredded** on power-off. Live changes seal back to the vault continuously.

The consequence: **moving out** and **cold backup** are nearly free (you already hold the key; the store is
just portable ciphertext). **Moving in** is the genuinely hard direction — and that's an Android platform
limit, not ours.

## i) Moving IN — from another (Android) phone

The hard direction. There is **no account to import**; "moving in" means **seeding your first session**,
which then seals into your vault.

- **Easy & standard (your *content*):** contacts via vCard, photos / media / files copied in over USB/SD,
  cloud-backed apps just re-signed-into (Gmail, etc. re-sync themselves). All of this works in the live
  session and roams from there.
- **Hard (app *data*):** cloning another app's `/data` off a stock Android phone is blocked by Android's
  per-app sandbox — without root or that app's own backup/cloud-sync you cannot extract it. This is why even
  Google's own switch tools lean on per-app cloud backup. A literal "clone my old phone" is not feasible for
  *anyone*; it isn't a Nowhere-specific gap.
- **Proposed feature — Import in the Create flow:** an optional step when creating a profile that pulls in a
  vCard + a media/files archive (or a Google Takeout export), populates the session, and lets the first seal
  carry it into the vault. Set expectations honestly: your **content** moves; your app installs + their data
  are **re-established fresh** (sign in again, restore per-app where the app supports it).

## ii) Moving OUT — to another phone / off Nowhere

**Easy, by design** — your data roams into a *real* Android user session, so when you're logged in it's all
just there in a normal session and standard export works: copy files/photos to USB/SD, export contacts to
vCard, etc.

- **No lock-in:** the vault is client-encrypted and **you hold the key** (name + passphrase). We *cannot*
  hold your data hostage — you can always decrypt it in a session and walk.
- **Proposed feature — "Export my data":** one tap in the Profile screen to bundle the session's
  media/contacts/files to an external drive or a portable archive, so leaving is turnkey rather than manual.

## iii) Cold backup / archive — self-hosted, no service fee

Where the architecture **shines**, and a real differentiator. The store holds **only client-encrypted
ciphertext** — plain S3-compatible blobs — so the user is never forced to pay anyone to keep their data.

- **Bring-your-own store — works *today*, no code:** the device conf points at *any* S3-compatible endpoint
  (`S3_ENDPOINT` / bucket / keys). Aim it at your own MinIO / NAS / self-hosted S3 → no fee, you own the
  bytes.
- **Cold archive:** because every object is ciphertext, you can snapshot the bucket to **anything** —
  Glacier, an external drive, a second NAS. To restore: copy back (or re-point at it), log in with
  name + passphrase, and it decrypts. Safe to archive *anywhere*, since the storage learns nothing.
- **Proposed feature — "Export / Import vault":** download the encrypted objects as one portable archive and
  re-point at a restored store, so the cold round-trip is turnkey instead of a manual `rclone`.

## Strategic note (for the commercialize phase)

The **managed store is a convenience, not custody**: anyone can self-host the store or cold-archive the
encrypted vault freely, and we never hold the key. That is a strong story against iCloud/Murena-style "your
backup lives only where we let it." The paid tier should sell **convenience + the least-knowledge gateway +
(eventually) unlinkable billing — never custody of your bytes.** Import (move-in) is the one place to invest
product effort, because export, BYO-store, and cold archive mostly fall out of the architecture for free.

See billing-model.md / commercialize.md for how this folds into the
paid offering.
