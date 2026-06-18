# Diaspore

**A stateless mobile OS for a roaming identity.**

Diaspore turns a phone into a thin terminal for an identity that lives in the
network. The operating system is **local and verified** — an ordinary,
content-addressed, OTA-updated LineageOS. Only your **user state** roams: app
list, app data, accounts, settings, and files are continuously encrypted and
replicated to a content-addressed store, restored when you log in, and **wiped
from the device on logout / power-off**. Blind-login on any Diaspore device →
your phone re-materializes. Power off → the device is a blank slate.

> A *diaspore* is the dispersal unit an organism scatters to regenerate elsewhere —
> and it quietly carries *diaspora*: your state living scattered across the network,
> never rooted to one device.

> The OS is stored **locally** (verified, content-addressed OTA — *not* netbooted).
> Only user state roams.

## The defining loop

1. **Boot** the local, verified OS; blind-login a profile (profile + passphrase →
   keys; a wrong cred or unknown profile is indistinguishable from a typo).
2. **Restore** your encrypted user state from the store into a fresh, ephemeral
   per-user `/data` — app *list* (app *code* is re-downloaded per device from
   Aurora/F-Droid), app data, settings, files.
3. **Run**, continuously replicating encrypted deltas to the store.
4. **Log out / power off** → the user's `/data` is sealed to the store, then
   wiped. The device holds nothing of yours at rest.

See `docs/roadmap.md` (plan) and `docs/app-model.md` (apps).

## Status

**Running on hardware — a Fairphone 3 (LineageOS).** Not concept; not emulator-only.

- **Arc 1 — identity / gate** ✅ — kiosk-locked blind-login chooser (device owner),
  Day-0 profile create, log off, and ROM self-provisioning. Proven on the FP3.
- **Arc 2 — per-user data roaming** ✅ — login spins up a fresh **ephemeral**
  Android user, a root daemon + `su:s0` worker restore the profile's encrypted
  `/data/user/N`, you run, and logout/power-off seals the delta then wipes the
  user; re-login restores it on a clean device. **App provisioning** rides along:
  the roamed identity's app set reinstalls onto the fresh user (not just its
  data). Proven on the FP3.

Earlier: the loop logic (a portable Phase 0 sim) and the Cuttlefish bring-up
(M1–M3: stock boot, custom stage-1 init, amnesiac `/data`) are done. Slice-by-slice
history is in [`worklog.md`](worklog.md).

**Next:** roam completeness (`/data/user_de`, `/data/media`), SELinux enforcing
(drop the bring-up `permissive`), verified boot (AVB), and the productionization
layer (store config/discovery, chunk-addressed sync, OTA, branding). The full
designed-but-not-built checklist lives in `docs/backlog.md`.

## Documents

- `docs/design.md` — full system design (architecture, components,
  flows, failure modes, tech choices).
- `docs/roadmap.md` — the implementation plan and phase ladder.
- `docs/app-model.md` — how apps roam (the list roams; code
  re-downloads per device).
- `docs/storage.md` — the client-encrypted, content-addressed,
  swappable store.
- `docs/enrollment.md` — Day-0 / profile creation and how
  store config lives on an amnesiac device.
- `docs/backlog.md` — the consolidated remaining-work checklist.
- `docs/boot-flow.md` — the initramfs network-root path, **kept
  for the optional Phase 5 netboot stretch** (the shipping model boots locally).
- `docs/prior-art.md` — survey of related work and what is
  genuinely unbuilt.

## Repository & workflow

Canonical home: **`git.linnae.ai/linnae/diaspore`** (self-hosted Gitea). Work
follows [`AGENTS.md`](AGENTS.md): a branch per slice (`dia-YYYYMMDD-NN-<topic>`,
or `codex/<topic>` for small no-ID cleanups), commits prefixed with the work ID,
and **never a direct commit to `main`**.

Separate from the Taxon and Lumen projects.

## Project identity (planned public presence)

These names are reserved for an eventual public launch (a website is a later
phase); today the project lives on the self-hosted Gitea above.

| Asset | Value |
|---|---|
| Domain | `diaspore.io` (recommended; `diaspore.dev` also free) |
| Public org | `diasporeos` |
| Packages | `diaspore` (crates.io / npm / PyPI — all free) |

## Accepted tradeoffs

An unlocked / custom-key bootloader is required, which forfeits Play Integrity /
SafetyNet and Widevine L1 — banking and DRM apps will not work. This is acceptable
for a privacy / amnesiac device.
