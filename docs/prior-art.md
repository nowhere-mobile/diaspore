# Diaspore — Prior art

Status: draft · 2026-06-08

Every individual piece of Diaspore already ships somewhere; what is unbuilt is the specific
combination (untethered, content-addressed, Android userspace, push-roaming-state-on-shutdown).

## Closest single analogue: postmarketOS "Duranium" + its netboot

- **Duranium** (March 2026) — an *immutable* postmarketOS: read-only core, updates applied
  as complete verified images, its own A/B slots.
- **Network boot** — merged for Fastboot devices. Mechanism, almost verbatim our sketch:
  **USB-Ethernet in the pre-init stage + NBD** to lazy-load the root block device from another host.

Gaps vs. Diaspore:
1. **Tethered** — USB-Ethernet to a host PC; "if the cable is unplugged the system stops."
   We want *untethered* (WiFi → a real distributed cache). That is exactly the
   two-stage-initramfs-does-WiFi step.
2. **pmOS (mainline Linux userspace), not Android/LineageOS.** But the boot transport
   (USB-eth + NBD in pre-init) is reusable regardless of userspace — a working reference
   for the boot stage.
3. **No push-roaming-state-on-shutdown** — pull-only.

## Mapped to the three requirements

**(i) Network-distributed / content-addressed OS image**
- **netboot.ipfs** — boot an OS from IPFS by content hash; an iPXE bootloader pulls
  kernel/initrd/rootfs from an IPFS gateway. Real (x86/PXE, not phone).
- **casync/desync** — the tool for "distribute a filesystem image + deltas over a CDN/cache,"
  content-defined chunking, CDN-friendly.
- **OSTree** — content-addressed, atomic, versioned bootable trees (Silverblue/Endless).
  Tradeoff: many small files are CDN-unfriendly, which is *why* casync exists.
- **HPC/diskless-at-scale:** xCAT, Warewulf, LTSP, NFS/NBD/iSCSI-root, netboot.xyz. Mature,
  not on phones.
- **Already in LineageOS:** Android A/B + dm-verity is already an immutable, hash-verified,
  read-only system image. We swap its *source* from a local partition to a distributed cache.

**(ii) Boot it on the phone**
- postmarketOS netboot (above) — the real one.
- `fastboot boot boot.img` — boot a kernel+ramdisk over USB without flashing (built into Android).

**(iii) Ephemeral local + wipe on reboot**
- **Tails** — the canonical amnesiac OS: leaves no trace, wipes on shutdown.
- **NixOS impermanence / "erase your darlings"** — tmpfs root wiped every boot, with an
  allowlist of what persists. The cleanest model for the overlay design.
- **live-boot/casper** (Ubuntu/Debian live) — squashfs-lower + tmpfs-upper overlay, productized.

## The genuinely unbuilt part

Nobody has combined:
1. **Untethered content-addressed netboot on a phone** — pmOS is tethered-to-PC;
   netboot.ipfs is x86. Over WiFi from a real distributed cache is the unsolved seam.
2. **Push-roaming-state-back-on-shutdown.** The most novel piece. Everything above is
   pull-and-discard (amnesiac) or pull-and-update (immutable OS). "Snapshot mutable state,
   encrypt it, push it to the distributed store, then wipe" has no clean OS-level analogue —
   closest cousins are roaming user profiles and config-sync (Windows roaming profiles,
   chezmoi, Nextcloud), which operate at the file/app layer, not whole-`/data`.
3. **On Android/LineageOS userspace specifically** — the immutable/ephemeral work is
   overwhelmingly mainline-Linux (pmOS, NixOS, Silverblue), not Android.

Verdict: ~80% existing, shipping primitives; the ~20% that is new (untethered WiFi netboot +
encrypted roaming-state-on-shutdown for Android) is the hard, interesting part — and the
boring plumbing has reference implementations to crib from.

## Sources

- postmarketOS: Introducing Duranium (immutable) — https://postmarketos.org/blog/2026/03/17/introducing-duranium/
- postmarketOS network/live boot writeup — https://tuxphones.com/postmarketos-linux-live-usb-fastboot-network-boot/
- netboot.ipfs (boot OS from IPFS) — https://github.com/magik6k/netboot.ipfs
- casync — distributing filesystem images — https://0pointer.net/blog/casync-a-tool-for-distributing-file-system-images.html
- casync on LWN — https://lwn.net/Articles/726625/
- OSTree related projects / tradeoffs — https://ostreedev.github.io/ostree/related-projects/
- NixOS impermanence — https://github.com/nix-community/impermanence
- Three years of ephemeral NixOS — https://b.tuxes.uk/three-years-of-ephemeral-nixos.html
- netboot.xyz — https://netboot.xyz/
