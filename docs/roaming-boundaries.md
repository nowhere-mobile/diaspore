# Diaspore — Roaming Boundaries (what roams, what can't)

Status: design note · 2026-06-11

Diaspore's promise is "your data roams; the device holds nothing at rest." This note draws the line
precisely: **Diaspore roams data; it cannot roam hardware-rooted trust.** That boundary isn't a bug to
fix — it's two security models colliding — so it's written down once, clearly, instead of rediscovered
every time someone asks "but does *my* app work?"

## The two layers

### 1. Roamable — "your stuff" (the ~90%)
Anything that is just *data on the device* roams cleanly: sealed into the content-addressed store on
sync, restored on login, wiped on power-off.
- Messages (SMS/MMS), call log, contacts — they live in system content providers
  (`com.android.providers.telephony` → `mmssms.db`; `com.android.providers.contacts` →
  `calllog.db`, `contacts2.db`).
- Files, photos, downloads, notes, settings.
- Most app data under `/data/data/<pkg>` and `/data/user/N`.

The clean mechanism for full fidelity is the **per-user model** (see the session-lifecycle item in
backlog.md): make each identity an Android user and roam its entire `/data/user/N`, which already
contains the providers and every app's data — Android does the per-user isolation and the provider/app
lifecycle for us; Diaspore just seals/restores the per-user blob.

### 2. NOT roamable — device-bound trust (the ~10%)
A whole class of apps *deliberately* anchor themselves to the physical device, for security. These do
not roam, by design:
- **Android Keystore keys** — hardware-backed, sealed in the TEE/StrongBox; built to never leave the
  chip. A roamed copy of an app is missing the one key that makes its secrets valid.
- **Device attestation (Play Integrity / SafetyNet)** — proves "genuine, unmodified device"; banking
  and payments gate on it. A roamed identity changes the attestation — and this build (LineageOS +
  unlocked bootloader) **already FAILS Play Integrity**, so those apps are limited here regardless of
  roaming.
- **Server-side device registration** — apps that bind your account to one device (WhatsApp, Signal):
  moving the data to a new device triggers re-verification (an SMS code), and rapid device-hopping
  trips anti-fraud.
- **DRM** — Widevine L1 keys are per-device; roamed media licenses won't validate.

## Worked examples
| App / data | Roams? | Why |
|---|---|---|
| SMS / call log / contacts | **Yes** | Provider databases — pure data (per-user model). |
| Files, photos, notes, settings | **Yes** | Pure data. |
| Most app data (games, readers…) | **Mostly** | `/data/data` — unless it leans on Keystore. |
| WhatsApp / Signal | **Data yes, login no** | Chats roam (file-encrypted DB); registration is device-bound → re-verify per device. |
| Banking / payments | **No** | Play Integrity + Keystore; also fails on this ROM's unlocked bootloader. |
| Netflix (HD), other DRM | **No** | Widevine L1 device keys. |
| Authenticators / passkeys | **No** | Device-bound credentials (that's the point of them). |

## Design stance
- Diaspore **owns the data + privacy story**: messages, contacts, files, settings, most apps, and the
  amnesiac "nothing left at rest" — on power-off your SMS history *leaves the device*, a stronger
  property than any normal phone offers.
- Diaspore is **honest about the boundary**: device-anchored trust (banking, DRM, attestation, the
  WhatsApp/Signal login handshake) re-attests or re-registers per device. We don't pretend to defeat
  attestation — that's a different, adversarial problem.
- Framing: **roamable data is your life; device-bound trust is the stuff someone deliberately nailed
  down.** Diaspore moves the first and is upfront about the second.

## Implications for the build
- The **per-user model** is the right home for full-fidelity data roaming (SMS/contacts/app data) and
  reconciles with the session-lifecycle (logoff/switch) design.
- **Volume**: app data is GBs, and today's roaming state is a **RAM-backed tmpfs** — right for a small
  working set, wrong for a full `/data`. Full roaming needs a disk-or-hybrid backing + chunk-level CDC.
- For device-bound apps the realistic UX is: the data is there, but the app re-registers/re-attests on
  first use per device. Set that expectation rather than promising seamlessness.
