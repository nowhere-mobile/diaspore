# Diaspore — Identity model (as built): names, Create vs Unlock, recovery codes, delete

Status: as-built · 2026-06-16 · pairs with [enrollment.md](enrollment.md), [recovery.md](recovery.md),
[roaming-boundaries.md](roaming-boundaries.md)

This documents how a Diaspore *identity* actually behaves at the gate — the parts that surprise people
because they assume a username/password account model. It is not that. Captured after a debugging session
(DIA-20260616-50) where the questions below came up.

## An identity is `name + passphrase`, together — not a "username with a password"

A profile's identity is a one-way hash of **both** the name and the passphrase:

```
master       = Argon2id(passphrase, salt = sha256("diaspore-salt:" + name))
profileRef   = hex(HKDF(master, "diaspore-ref"))   # the store key for this identity's head
```

Consequences (all intentional):

- **The name is not unique and is not an account.** It's just a label that, *combined with the
  passphrase*, derives the ref. `demo / pass-A` and `demo / pass-B` are **two completely independent
  profiles** — different data, different recovery codes, zero relationship. Two different people can both
  use the name "demo" with different passphrases and never collide; one person can keep several profiles
  under one name.
- **There is no username namespace to enumerate.** You cannot ask "does profile `demo` exist?" — only
  `(name, passphrase)` together resolves to a ref, and the ref is unguessable without both. This is what
  makes the gate **blind**: it is never an oracle for whether an identity exists.
- A **wrong passphrase** derives a *different* (unguessable, non-existent) ref — indistinguishable from a
  name that was never created.

## Two gate paths: Create vs Unlock

| | **Create a profile** | **Unlock** |
|---|---|---|
| Daemon path | `handleCreate` → `createVault` | `handleRoamIn` → roam worker `restore` |
| Writes to the store? | **Yes** — writes the head ref (`profileRef → vault`) + recovery ref | **No** (read/restore only) |
| Recovery code? | **Yes** — minted once, shown once | Only if this login does a one-time legacy→vault **migration** (see below) |
| Result if it doesn't resolve | n/a (it's creating) | a **blind empty session** (you get an empty phone) |
| Persists? | Yes — it's enrolled | An Unlock into an **existing** profile persists its updates; an Unlock into a **non-existent** name+pass is **ephemeral** (see below) |

**Why Unlock into a never-created name+pass gives an empty session, not an error:** blindness. The gate
must not reveal that the profile doesn't exist, so a non-resolving Unlock lands you in an empty ephemeral
user — identical to unlocking a real-but-empty profile. The roam worker's `restore` of a non-resolving
profile prints "empty → nothing" and **exits 0**, so roam-in reports OK (empty), not BLANK. The gate only
shows **"Couldn't sign in"** on an actual **error** (no internet, store unreachable) — never on a
non-resolving name+passphrase.

**So: Create is the only way to make a real, persistent, recoverable identity.** Unlock opens an existing
one, or hands you a throwaway blank.

## Why a recovery code *sometimes* appears on Unlock (and why that's an artifact)

A recovery code on **Unlock** comes from exactly one place: `handleRoamIn` calls `migrateVault`, which
mints a fresh code **only if the profile resolves AND is in the old pre-vault (legacy) format** — a
one-time **legacy → vault upgrade**. A genuinely non-existent name+pass has no head → no migration → no
code.

So "a recovery code showed up on a profile I thought was new" means the profile **already existed as a
legacy head** and this login upgraded it. How did a "new" profile come to exist as legacy?

> **Historical bug (fixed DIA-20260616-50):** before the fix, Unlocking a brand-new name+pass would
> *silently persist* it — the first background **seal** (`push`) saw no head and **created one**, as a
> bare *legacy* profile (no vault, **no recovery code**). So any name+pass you'd ever Unlocked got saved
> as a legacy head; the *next* Unlock resolved it → migrated → a recovery code appeared "from nowhere."

This happens **at most once per profile** (after migration it's a vault → no more code on Unlock). After
the fix, Unlocking a genuinely new name+pass **no longer persists anything**, so it can't quietly become a
legacy profile and surprise you later. Going-forward rule: **recovery codes come from Create** (plus a
one-time migration for the legacy stragglers).

## "Delete my profile" semantics

`deleteProfile(name, pass)` (the gate's logoff → "Delete this profile", and the `delete-profile` CLI):

1. Drops the head ref **and** the recovery ref so `(name, passphrase)` no longer resolves. On the store we
   **tombstone (overwrite the ref to empty)** rather than hard-delete the key — only the tombstone is
   strongly consistent on the IPFS-backed store, and a hard `RemoveObject` could be denied by a
   write-scoped key or serve stale (the original "deleted but still logs in" bug). It deletes **only if
   `(name, passphrase)` resolves** — a wrong passphrase is a no-op (can't wipe someone else's profile).
2. Reaps the **local** roamed session **without a final seal** (so the just-deleted profile isn't
   re-uploaded).
3. The continuous **seal must never resurrect** a deleted profile: `push` skips a bare profile whose head
   is gone (it was deleted; `create-vault` always writes the head before any seal, so a missing head means
   deletion). This was the real "demo kept coming back" bug — the live session's periodic seal was
   re-creating the tombstoned head.

**After deletion, signing in with the same name+passphrase still "works" — into a blind *empty* session.**
That is the blind model again: the gate can't reject it (that would leak that the profile was deleted), so
you get a fresh empty phone. The user-facing point of delete is that the **data is destroyed**; the
name+passphrase becomes equivalent to a never-created one.

### Why the logoff screen keeps "Delete this profile" (and "Change passphrase") even for a throwaway session

A throwaway session — an Unlock into a name+pass that was never created (or a wrong passphrase) — has **no
stored profile**, so "Delete this profile" there is a no-op (`deleteProfile` finds no head → `NOTFOUND` →
"couldn't delete — check your passphrase"); same for "Change passphrase". It's tempting to **hide** those
options for such sessions — but that would **break deniability**, so we deliberately don't.

The gate is built so a *real* login, a *wrong-passphrase* login, and a *never-existed* login are
**indistinguishable** — all three drop you into a session. If the logoff screen showed Delete/Change only
for *real* profiles, the screen itself would become an existence **oracle**: anyone watching or coercing a
logged-in session could tell whether the name+passphrase resolved to a real stored profile or was a
decoy/empty one. So the logoff screen shows the **same options regardless**, and the failure message is
deliberately **uniform** — deleting a throwaway (nothing there) and deleting a real profile with the
*wrong* passphrase both say "couldn't delete — check your passphrase". The screen looks and behaves
identically whether or not a real profile backs the session.

(There's also a mechanical reason it *can't* cheaply tell: LogoffActivity only knows the user's **name**,
and by design the roam-in never surfaces whether the login resolved a real profile vs an empty one — that
blindness is the point. The only cost of all this is a slightly confusing message in the throwaway edge
case, which is the deliberate price of keeping the screen blind.)

## The blind rule, summarized

Wrong passphrase, deleted profile, and never-existed name are **all indistinguishable** at the gate — each
yields an empty session. Only an operational error (offline / store down) yields a visible failure. There
is no profile-existence oracle anywhere in the system; that is the whole point.
