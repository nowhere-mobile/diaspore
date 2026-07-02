# Diaspore — Enrollment & Configuration (the "Day 0" flow)

Status: design · 2026-06-10 · pairs with [storage.md](storage.md); UI extends the P3.1 chooser

> For the **as-built** identity semantics — why names aren't unique, Create vs Unlock, when a recovery code
> appears, and what "delete my profile" actually does — see [identity-model.md](identity-model.md).

The blind-login chooser (P3.1) answers **"Day N"** — *unlock an existing profile*. This doc covers
**"Day 0"** — *create a profile, set a recovery key, and choose where your data is saved.* It's the
counterpart we haven't built yet, and it also surfaces two crypto refinements the unlock-only path
let us skip.

## The gap

Today the chooser only **unlocks**: name + passphrase → derive keys → restore. There is no path to
**create** a profile or **configure the store**, and the demo crypto has two simplifications that are
fine for one user but wrong for production (collisions, no recovery). Enrollment fixes all of this.

## 1 — Create a profile (name + passphrase)

A "create" mode on the chooser:

1. Enter desired **profile name** + **passphrase** + confirm. (Blind: nothing is listed; a
   **hidden/duress** profile is created *identically* — that's what makes it deniable.)
2. Derive the key material (below) and **initialize an empty manifest** in the store → the profile now
   exists and is immediately usable.
3. Show the **recovery key** (below) and require the user to record it.

Mechanically this is a small extension of the agent (which already derives keys + pushes manifests)
and the chooser app (add a "Create" path next to "Unlock").

## 2 — Recovery key (non-optional)

The store is zero-knowledge, so **a forgotten passphrase = permanently unrecoverable data** unless we
plan for it. Fix: don't encrypt data directly with the passphrase-derived key. Instead:

- Generate a random **data key `DK`** (the thing that actually encrypts the manifest/blobs).
- **Wrap** `DK` twice and store both wrapped copies in the (ciphertext) head:
  - `wrap(DK, KEK_pass)` where `KEK_pass = Argon2id(passphrase, salt(name))`
  - `wrap(DK, KEK_recovery)` where `KEK_recovery` = a high-entropy **recovery key** shown once at
    enrollment (e.g. a BIP39-style mnemonic the user writes down).
- To open: unwrap `DK` with **either** the passphrase **or** the recovery key.

This is the standard recovery-key/key-wrapping pattern (FileVault/BitLocker-style), and it also gives
**passphrase rotation for free** — change the passphrase by re-wrapping `DK` under a new `KEK_pass`,
*without* re-encrypting any data. (The current demo encrypts directly with `Argon2id(pass)` → no
recovery, and a passphrase change would mean re-encrypting everything. The DK-wrapping design is the
production fix.)

## 3 — Where data is saved (store config on an amnesiac device)

This is the subtle one, because **the device wipes `/data`** — so "which store" can't live there.
Two layers:

- **Backend choice** (managed default / self-hosted URL+creds / IPFS — see [storage.md](storage.md))
  is picked during enrollment.
- **Where that choice lives** has two designs:

| | **v1 — device-level config** | **target — discovery/bootstrap** |
|---|---|---|
| Where store config lives | OS/device layer (persists; only `/data` is wiped) + a built-in **default** | nowhere on the device — resolved from name+passphrase |
| Roam to a new device | works if devices share the store / default; self-hosted = set per device (or scan a QR) | **seamless** — any device pointing at the same discovery endpoint re-materializes you, store and all |
| New dependency | none (just a default) | one tiny **discovery endpoint** (zero-knowledge KV, replicable) |
| Build cost | low | one indirection on top of the existing crypto |

**Discovery/bootstrap, concretely:** name+passphrase → a deterministic **bootstrap ref** → `GET` at a
well-known discovery endpoint → returns your **sealed store-config** → unseal → now talk to your real
store. The discovery endpoint only ever sees opaque refs → ciphertext, exactly like the data store, and
can be replicated. Enrollment just `PUT`s the sealed store-config at your bootstrap ref.

Because the GET only ever returns sealed, zero-knowledge ciphertext at an unguessable ref, the **lookup can
be anonymous** — point the device at a **public-read** discovery bucket with *no credentials* and a fresh
device bootstraps with **zero baked creds** (DIA-20260615-44: `discoCanLookup()` = endpoint+bucket; an
anonymous HTTP GET fetches the sealed blob; only **publish** needs write creds). That removes the
leaked-image-key risk from the bootstrap path; scoped **write** tokens are the remaining Phase-2 broker step.

**Plan:** ship **v1 (device-level config + default)**; add the **discovery layer** as the upgrade that
makes roam-to-any-device seamless.

## 4 — Ref binding (a correctness fix enrollment forces)

Today `ref = sha256("diaspore-ref:" + name)` — *name only*. Two users picking "alice" collide on the
same head. Production must bind the ref to **name + passphrase** so distinct identities get distinct,
**unguessable** refs (which also strengthens deniability — you can't even probe whether a name exists
without its passphrase). Clean derivation, with key separation:

```
master      = Argon2id(passphrase, salt = sha256("diaspore-salt:" + name))
ref         = hex( HKDF(master, "diaspore-ref") )        # storage location (unguessable)
KEK_pass    =      HKDF(master, "diaspore-kek")           # wraps DK (see recovery)
bootstrapRef= hex( HKDF(master, "diaspore-bootstrap") )   # discovery lookup
```

One `master` per (name, passphrase); separate sub-keys for naming, key-wrapping, and discovery — never
the same key for two purposes.

## 5 — Enrollment rate-limit (anti-flood)  *(DIA-20260615-41)*

Create is **open** — the gate has no profile list and no admin step, so anyone at the device can mint a new
`(name, passphrase)`. That's by design for the blind/anonymous model, but it means a script at the gate could
flood the store with junk heads/blobs (storage abuse / quota drain). The agent throttles `CREATE` with a
**per-device token bucket**:

- **Capacity** `DIASPORE_ENROLL_MAX` (default **5**), **fully refilled over** `DIASPORE_ENROLL_WINDOW`
  seconds (default **3600**). Refill is continuous (`MAX/WINDOW` tokens/s), so a fresh device allows a burst
  of `MAX` — normal first-run setup — then ~`WINDOW/MAX` between creates.
- **State** is a tiny JSON `{tokens, ts}` at `/data/diaspore/enroll`, the same persistence class as the
  rollback anchor and the conf: it **survives the power-off wipe** (only the tmpfs `state` dir is RAM), so an
  attacker can't reset the limit by power-cycling. Only a factory reset clears it.
- Checked in `handleCreate` **after** the `NOSTORE` guard and **before** any store I/O, so a throttled flood
  never even issues store requests. On denial the daemon replies `RATELIMIT <retry-seconds>` and the gate
  shows *"too many new profiles — try again in N min"*. Login/unlock is **not** limited (the blind gate
  already makes wrong-pass attempts expensive + indistinguishable).
- `DIASPORE_ENROLL_MAX=0` **disables** it (turnkey / trusted single-user devices).

Scope: this is a **local, one-device** throttle. It does not stop a distributed attacker with many devices —
that's the job of the **store-side enrollment gate / scoped credentials** (a Phase-2 least-knowledge broker
that issues per-enrollment tokens), tracked separately under security hardening.

## UI — extend the P3.1 chooser

The chooser app grows three entry points (still blind — no profile list):

- **Unlock** (exists) — name + passphrase → restore.
- **Create** — name + passphrase + confirm → recovery key → init manifest → (pick store).
- **Settings / Store** — view/set the device's store or discovery endpoint + backend choice.

Device-level defaults (the discovery/store endpoint) are **OS config set at provisioning**, not user
`/data`, so they survive the wipe.

**Turnkey baked default (DIA-20260615-42).** For a *turnkey* device the discovery endpoint is baked into
`/system/etc/diaspore/discovery.conf` so a **freshly flashed / factory-reset** device needs no manual store
setup at all: device-owner + kiosk self-provision (`diaspore_provision.sh`), and on the first login the
daemon falls back to the baked discovery config and re-materializes the profile from name+passphrase. The
bake is **opt-in + conditional** (`diaspore.mk` `$(wildcard vendor/diaspore/etc/discovery.conf)`): the real
`discovery.conf` is `.gitignore`d (only `*.example` is tracked), so a **clean checkout builds an un-enrolled
OS** and creds never enter git. Trade-off: it writes the `DISCO_*` endpoint creds into `/system` — fine for
a managed device with a **scoped, least-privilege** discovery key, but the dev caveat (account-level key)
stands until the Phase-2 token broker.

## Enrollment ↔ unlock state machine

```
first boot on a device ─ device has a store/discovery endpoint (default or set in Settings)
        │
        ├─ Create ─ new (name,passphrase) ─ make recovery key ─ init empty manifest ─► usable, empty
        │
        └─ Unlock ─ existing (name,passphrase) ─ resolve ref ─ restore working set ─► usable, your data
                         │
                         └─ wrong pass / unknown name ─► identical blank  (blind login, unchanged)
shutdown ─► push final delta ─► wipe /data  (unchanged)
```

## What lives where (recap)

| Thing | Where | Wiped on power-off? |
|---|---|---|
| OS + agent + chooser | local OS image | no (it's the OS) |
| Store / discovery endpoint (device default) | OS/device config | no |
| Profile name + passphrase | **only in the user's head** | n/a (never stored in clear) |
| Recovery key | written down by the user at enrollment | n/a |
| Data key `DK`, wrapped by pass + recovery | in the sealed head, in the store | n/a (it's in the store) |
| App data / settings / files | encrypted blobs in the store; restored to `/data` | yes (`/data`) |

## Open items / build plan

- [ ] Chooser app: **Create** path (name+pass+confirm) + **Settings/Store** screen (extends P3.1).
- [ ] Agent: `create` command (init empty manifest) + **DK key-wrapping** (pass-KEK + recovery-KEK).
- [ ] Recovery key: generate + display (BIP39-style) + an "unlock with recovery key" path.
- [ ] Ref binding: derive `ref`/`bootstrapRef` from `master` (HKDF), not from name alone.
- [ ] Passphrase rotation: re-wrap `DK` under a new `KEK_pass`.
- [ ] (target) Discovery endpoint: a tiny zero-knowledge KV (`bootstrapRef → sealed store-config`) +
      the device default + the `PUT`-on-enroll / `GET`-on-unlock paths.
- [ ] Device provisioning: how the OS image ships its default store/discovery endpoint.
