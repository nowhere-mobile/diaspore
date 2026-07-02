# Nowhere — architecture & threat model

Status: living security doc. Covers both editions — **Diaspore** (Fairphone / LineageOS) and **Endospore**
(Pixel / GrapheneOS-derived); edition differences are called out. This is the honest companion to the
**Verify** page: what the design defends against, what it does **not**, and the assumptions it rests on.

## 1. The model in one paragraph
A nowhere phone runs a **local, verified OS**. Your identity — apps, app data, accounts, messages, files,
settings — lives **encrypted in a content-addressed object store**, not on the handset. You log in with a
name + passphrase; your keys are derived **on the device** and the session re-materializes into a fresh,
ephemeral space. On power-off the session is sealed to the store and the device is **wiped** — at rest it
holds only the OS, none of your plaintext. Storage is paid for with **blind-signed tokens**, so the gateway
that meters usage can't link it to the payment.

## 2. Components & trust boundaries
```
  DEVICE (trusted; keys in RAM only)          STORE (untrusted; ciphertext only)
  ┌─────────────────────────────┐   reads /   ┌──────────────────────────────┐
  │ verified OS + gate/chooser  │   writes    │ content-addressed objects     │
  │ agent (crypto, roam, seal)  │◀──via caps─▶│ (encrypted blobs at hashes)   │
  │ DK + passphrase (RAM/tmpfs) │             └──────────────────────────────┘
  └──────────────┬──────────────┘                          ▲
                 │ control plane (tiny JSON)                │ short-lived presigned caps
                 ▼                                          │
  GATEWAY (least-knowledge: refs / sizes / epochs; NO identity; blind-token billing)
                 │  one-way blind issuance — the two sides never join
                 ▼
  STOREFRONT / identified side (PII, payments, tax records) — a separate domain
```
- **Device** — *trusted*. Holds the passphrase + derived keys **only in RAM / tmpfs**, wiped on power-off.
- **Store** — *untrusted*. Holds only **ciphertext** at content-addressed refs; never sees keys or plaintext.
- **Gateway** — *least-knowledge*. Sees pseudonymous refs / sizes / epochs + blind-token redemptions; **no
  identity**, and by the blind-token math it cannot link usage to payment.
- **Storefront / identified side** — holds PII + payments (tax-retained), **physically separate** from the
  gateway; the blind issuance is the only, one-way bridge between paying and storing.

## 3. Keys
A random **Data Key (DK)** encrypts your data (AES-256-GCM). DK is wrapped in keyslots: a **`pass`** slot
(`Argon2id(passphrase, salt=name)`), a **`recovery`** slot (a 12-word BIP-39 mnemonic), and on Endospore a
per-device **`se`** slot bound to the **Titan M2** (StrongBox). DK and the passphrase are derived on-device
and live **only in the running session's memory** — never sent to us, never written at rest.

## 4. Assets we protect
1. **Your content** (plaintext) — the primary asset.
2. **Your passphrase and keys.**
3. **The usage ↔ payment unlinkability** — that no one can tie *what you store* to *who paid*.
4. **OS integrity** — that the device runs the OS we published, untampered.

## 5. Adversaries — what they get, and don't
| Adversary | Gets | Does **not** get | Why |
|---|---|---|---|
| Thief, **powered-off** device | the hardware + the open-source OS | any of your data | power-off wipe; verified boot blocks silent OS tampering |
| Thief, **powered-on, locked** device | an FBE-ciphertext disk + a throttled unlock prompt | plaintext (without the passphrase) | idle cold-lock evicts the CE key → ciphertext at rest; the credential is hardware-throttled (Gatekeeper on FP3, Weaver on Endospore); a longer idle auto-wipes |
| **Store operator** / store breach | ciphertext blobs at hashes + sizes / timestamps | plaintext, keys, your identity | client-side encryption; the store never holds keys or PII |
| **Gateway operator** / breach | pseudonymous refs / sizes / epochs + unlinkable token redemptions | the usage↔payment link; your identity | blind-token issuance + redemption (the device blinds locally) |
| **Network** (MITM) | TLS-protected traffic | your content (even if TLS were broken) | TLS **plus** client-side encryption (defense in depth) |
| **Malicious OS update** | nothing useful | a forged OS on the device | signed A/B payload, verified boot under our key, rollback protection |
| **Legal process** | the identified side (billing / tax records) | useful store content; the usage↔payment link | the data-domain split; the store holds only ciphertext |
| **Us** (insider / "just trust us") | what the gateway/store see (ciphertext, pseudonymous refs) | your content; the usage↔payment link | client-side keys + **open, reproducible source** you can audit |

## 6. Assumptions & known limitations (the honest part)
- **Passphrase strength is load-bearing.** Because the store is client-encrypted, a captured blob is
  **offline-brute-forceable** against a weak passphrase. **Endospore** closes this — the Titan M2 throttles
  guesses and holds a non-extractable secret, so the passphrase is *not* offline-brute-forceable. **Diaspore
  / FP3** does **not** (a software KDF only) → choose a strong passphrase there. This is the central edition
  difference.
- **A live, compromised session is out of scope.** Keys are in RAM while you're logged in; root-level malware
  or a kernel exploit on a *running* session can read them. Amnesia protects the at-rest and powered-off
  states, not a runtime compromise.
- **Verified boot is *yellow*, under *our* key** — not Google's green. You trust **our** AVB key (and can
  verify the source it signs). Relock, rollback protection, and (Endospore) the secure element are genuinely
  enforced.
- **Traffic analysis.** The store sees object **sizes + timing**; size-bucketing / padding is a follow-on, so
  an operator can infer coarse activity patterns — never content.
- **Subscription billing is less unlinkable than prepaid.** The gateway holds `subHash↔budget`, and
  `subHash = H(subkey)` while the storefront holds `customer↔subkey` — so *"customer ↔ GB-months paid"* is
  joinable across the two. **What you store stays unlinkable** (the voucher→refill→token blind hops break it);
  only the billing relationship is visible. Prepaid (claim → blind tokens) has no such seam.
- **Telephony doesn't roam and isn't anonymous** — your phone number / SIM / carrier are outside the model.
- **The gateway & storefront are closed** (commercial). The unlinkability rests on the **client-side**
  blinding, which **is** open — so you can verify the device does what it claims; you don't have to trust the
  server's code for the privacy property to hold.

## 7. The two editions at a glance
- **Diaspore (FP3 / LineageOS):** amnesiac roaming + verified boot (yellow, our key) + the zero-knowledge
  store. No secure element → the passphrase is the whole story (pick a strong one).
- **Endospore (Pixel / GrapheneOS-derived):** all of the above **+ Titan M2** (passphrase hardware-throttled,
  non-extractable key custody) **+ GrapheneOS hardening** (`hardened_malloc`, MTE, exploit mitigations). The
  higher-assurance edition.

## 8. How to verify this (don't trust — check)
The OS, the client, and the architecture are open (Apache-2.0), and every release publishes its **source
snapshot + a signed image with checksums**. You can read the exact code for the version you run and
**rebuild it to confirm the image matches** — the claims above are verifiable, not asserted. See the
**Verify** page and the public source at **diaspore.io** / **endospore.io**.

*Out-of-scope here (separate docs): the recovery-key / keyslot design ([recovery.md](recovery.md)), the lock
model ([lock-model.md](lock-model.md) + [resumable-session.md](resumable-session.md)), roaming boundaries
([roaming-boundaries.md](roaming-boundaries.md)), and the secure-element binding ([se-binding.md](se-binding.md)).*
