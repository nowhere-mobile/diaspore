#!/usr/bin/env bash
# Phase 4 / P4.1: bootstrap the LineageOS build host for the Fairphone 3 (lineage-22.2).
# Installs build deps + the repo tool + repo init (shallow). The big `repo sync` is run separately
# (after the disk is grown to 400 GB).
set -uxo pipefail
SRC=/home/chesterr/android/lineage
BRANCH=lineage-22.2

echo "=== LineageOS build dependencies ==="
sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
  bc bison build-essential ccache curl flex g++-multilib gcc-multilib git git-lfs gnupg gperf \
  imagemagick lib32readline-dev lib32z1-dev libelf-dev liblz4-tool libsdl1.2-dev libssl-dev \
  libxml2 libxml2-utils lzop pngcrush rsync schedtool squashfs-tools xsltproc zip zlib1g-dev \
  fontconfig python3 python-is-python3
echo "deps rc=$?"

echo "=== repo tool + git identity ==="
mkdir -p ~/bin
curl -fsSL https://storage.googleapis.com/git-repo-downloads/repo -o ~/bin/repo
chmod a+x ~/bin/repo
git config --global user.name "Diaspore Builder"
git config --global user.email "chester.ragel@gmail.com"
git config --global color.ui false
git config --global trailer.changeid.key "Change-Id"

echo "=== repo init $BRANCH (shallow) ==="
mkdir -p "$SRC"; cd "$SRC"
~/bin/repo init -u https://github.com/LineageOS/android.git -b "$BRANCH" --depth=1 --git-lfs 2>&1 | tail -8
echo "=== .repo size ==="
du -sh "$SRC/.repo" 2>/dev/null | tail -1
df -h /
echo P4_1_INIT_DONE
