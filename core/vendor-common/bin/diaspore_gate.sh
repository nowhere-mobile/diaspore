#!/system/bin/sh
# Early gate launch -- the home-flash fix.
#
# The chooser's BootReceiver launches the gate on BOOT_COMPLETED, but that is an ORDERED broadcast with
# ~11 receivers that takes ~2s to drain, so the gate appears ~2s after launcher3 is already on screen --
# the visible "home flash" before the gate. This service instead runs the instant `sys.boot_completed`
# flips (init triggers it ~1s BEFORE the broadcast is even posted), racing the gate up with the home so
# the launcher is barely visible.
#
# su:s0 (like diaspore_provision) so `am` can start an activity. Idempotent with the BootReceiver:
# ChooserActivity is singleTask, so a duplicate launch is a no-op. Also clears the Android-12+ "stopped"
# state of a freshly-flashed system app on EVERY boot (not just first-provision), so the gate auto-arms.
i=0
while [ "$i" -lt 4 ]; do
  am start -n com.diaspore.chooser/.ChooserActivity >/dev/null 2>&1 && break
  i=$((i + 1)); sleep 1
done

# Re-assert keyguard-off on EVERY boot. The one-time provisioning `locksettings set-disabled true` does
# NOT reliably persist across reflashes/boots (observed get-disabled=false), which lets a swipe-keyguard
# precede the gate. Re-applying here (su:s0, every boot) keeps the boot landing straight on the gate.
locksettings set-disabled true >/dev/null 2>&1
