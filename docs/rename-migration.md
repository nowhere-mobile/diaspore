# Nowhere — package rename is a device-owner migration

Status: design · 2026-06-23 · pairs with [boot-flow.md](boot-flow.md) / the provisioning
(`core/vendor-common/bin/nowhere_provision.sh`).

The chooser is the Android **Device Owner** (DO). The DO is **bound to the package name**, so renaming the
chooser package (the `com.diaspore.chooser` → `com.nowhere.chooser` rename) is a **breaking change for any
device already provisioned under the old name** — it cannot be fixed by a reflash; it needs a **factory
reset**. This doc records why, the migration, and the policy that follows.

## Why a rename breaks an already-provisioned device

- Android allows **exactly one Device Owner**, settable only on an **unprovisioned** device, and there is
  **no cross-package transfer** (`transferOwnership` moves ownership between admins *within the same
  package*, not between packages).
- The DO record lives in **`/data/system/device_owner_2.xml`** (a `package="com.diaspore.chooser"` entry).
  A **system-only reflash does not touch `/data`**, so after flashing a `com.nowhere` build:
  - `/data/system/device_owner_2.xml` **still names `com.diaspore.chooser`** (a package that no longer
    exists in the new image);
  - first-boot provisioning runs `dpm set-device-owner com.nowhere.chooser/.AdminReceiver`, which **fails**
    ("already a device owner");
  - `owner_ok()` (which matches `com.nowhere.chooser`) stays false → the gate **never kiosks**, the device
    is stuck as a non-Lock-Task gate.
- The stale DO **cannot be cleared from outside** the owning app: `clearDeviceOwnerApp()` can only be called
  *by* the DO app — and that app (`com.diaspore.chooser`) is gone. So there is **no in-place fix**.

## The migration: factory reset (and that's fine — data is safe)

For a device provisioned under the old name, the only path is a **factory reset**, which wipes `/data`
(including the stale DO record). Then:

1. Factory reset → `/data` wiped → first boot.
2. `nowhere_provision.sh` (fresh `/data`, no marker) runs `dpm set-device-owner com.nowhere.chooser` →
   **succeeds** (no existing DO) → the gate kiosks normally.
3. Re-enter **Wi-Fi** + the **store config** at the gate (Settings → Store), or re-provision
   `/data/nowhere/nowhere.conf`.
4. Each user **logs in** → their vault **roams back** from the store.

### What is lost vs. safe

| Survives the reset? | Item | Notes |
|---|---|---|
| ✅ **Safe** | **All user data** | It lives in the client-encrypted store, not on the device — logging in roams it back. This is the amnesiac model working *for* us. |
| 🔁 Re-done (automatic) | Device-owner / kiosk provisioning | `nowhere_provision.sh` re-runs on the fresh `/data`. |
| 🔁 Re-done (manual) | **Store config** (`S3_ENDPOINT`/keys) | Re-enter at the gate or re-provision the conf. **Back it up before the reset** (the admin already holds it; it's never committed). |
| 🔁 Re-done (manual) | Wi-Fi | Re-enter at the gate. |

So a rename migration costs the **device's local config** (re-entered), **never the user's data**.

## Policy (the important takeaway)

- **`com.nowhere.chooser` is now the permanent package id.** Do **not** rename the chooser package again
  after any real device ships — a rename is a **fleet-wide factory-reset migration**.
- The `diaspore → nowhere` rename was done in the **dev/pilot phase (FP3 only, no fielded devices)**, so its
  migration cost was effectively nil (the dev FP3 was factory-reset once and re-provisioned). This doc exists
  so the constraint is understood **before** there are devices in the wild.

## Provisioning aid (graceful detection)

`nowhere_provision.sh` now detects a **foreign device-owner** (a DO record present that is **not**
`com.nowhere.chooser`) and writes a clear, actionable line to `provision.log`
("FOREIGN device-owner … factory reset required") instead of silently burning the retry budget and leaving a
non-kiosk gate. It also **skips** the (doomed) `set-device-owner` retry loop in that case.

## If a rename were ever unavoidable on a real fleet (not pursued)

A factory-reset-free path would need a **transitional build** that still ships the *old* package as DO, has
it call `clearDeviceOwnerApp()` to release ownership, then a follow-up build under the new name claims the
now-unowned device. This is fragile (a window with no DO → no kiosk; two coordinated OTAs) and unnecessary
for the one-time `diaspore → nowhere` move, so it is **documented but not implemented**. The standing
recommendation is: **don't rename**; if you must, **factory-reset migrate** with the reassurance that user
data roams back.
