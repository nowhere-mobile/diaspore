#!/usr/bin/env bash
# Phase 4 / P4.1: format + mount the pd-standard build disk (google-build = sdb) and move the
# repo-init'd tree onto it. Refuses to touch the root disk.
set -uxo pipefail
DEV=/dev/disk/by-id/google-build
RES=$(readlink -f "$DEV"); echo "build disk: $DEV -> $RES"
case "$RES" in
  */sda|*/sda[0-9]*) echo "REFUSING: $RES is the root disk"; exit 9;;
esac
if [ -z "$(lsblk -no FSTYPE "$RES")" ]; then
  echo "formatting $RES ext4 (no reserved blocks)..."
  sudo mkfs.ext4 -F -m 0 -L diasporebuild "$DEV"
else
  echo "filesystem already present on $RES; skipping format"
fi
sudo mkdir -p /mnt/build
grep -q ' /mnt/build ' /etc/fstab || echo "$DEV /mnt/build ext4 defaults,nofail 0 2" | sudo tee -a /etc/fstab >/dev/null
mountpoint -q /mnt/build || sudo mount "$DEV" /mnt/build
sudo chown -R chesterr:chesterr /mnt/build
mkdir -p /mnt/build/lineage
# relocate the repo-init metadata from the (small) SSD home onto the build disk
if [ -d /home/chesterr/android/lineage/.repo ] && [ ! -e /mnt/build/lineage/.repo ]; then
  mv /home/chesterr/android/lineage/.repo /mnt/build/lineage/.repo
fi
echo "=== df ==="; df -h /mnt/build
echo "=== .repo on build disk? ==="
ls /mnt/build/lineage/.repo/manifests/ >/dev/null 2>&1 && echo REPO_OK || echo REPO_MISSING
echo BUILD_DISK_READY
