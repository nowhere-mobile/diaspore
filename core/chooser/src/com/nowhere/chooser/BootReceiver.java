package com.nowhere.chooser;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;

/**
 * Launches the blind-login chooser once the device finishes booting -- the login front door.
 *
 * Safe by construction: a fresh boot lands on the gate with an EMPTY roaming tmpfs -- there is no
 * auto-restore (the Arc-1 baked-identity baseline is gone), and launcher3 stays a fallback home, so a
 * failure here can't bootloop or strand the device. The restore happens only on an explicit blind login.
 */
public class BootReceiver extends BroadcastReceiver {
    @Override
    public void onReceive(Context context, Intent intent) {
        if (Intent.ACTION_BOOT_COMPLETED.equals(intent.getAction())) {
            final Context app = context.getApplicationContext();
            // Bring the gate up IMMEDIATELY -- never wait on the wipe (no home-flash, and no ANR if it stalls).
            // NO_ANIMATION + bring-to-front: come up at once over launcher3 (shown at boot_completed).
            Intent i = new Intent(app, ChooserActivity.class);
            i.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK
                    | Intent.FLAG_ACTIVITY_NO_ANIMATION
                    | Intent.FLAG_ACTIVITY_REORDER_TO_FRONT);
            app.startActivity(i);
            // Wipe leftover roamed users OFF the main thread (#47): the wipe makes BLOCKING daemon socket calls
            // (cleanUserStorage -> pollDaemon, up to 5s each), and running them inline on this receiver's main
            // thread froze the gate into a 5s ANR ("nowhere isn't responding") whenever the daemon was slow to
            // answer right after boot. goAsync keeps the process alive until the background wipe finishes; the
            // gate's onResume orphan-sweep (#73) is a second net if the process dies mid-wipe.
            final PendingResult pr = goAsync();
            new Thread(() -> {
                try {
                    wipeLeftoverRoamedUsers(app); // P2: amnesiac-on-(unclean)-power-off + free the tight 4-user cap
                } finally {
                    pr.finish();
                }
            }, "nowhere-boot-wipe").start();
        }
    }

    /**
     * P2 (DIA-20260625-12, docs/resumable-session.md): on every boot, crypto-shred any leftover roamed user.
     *
     * With the resumable model the roamed user is NON-ephemeral, so a clean logoff removes it but an UNCLEAN
     * power-off (battery death) — or a cold-locked idle session that was never logged off — leaves its encrypted
     * /data on disk across the reboot. This restores the amnesiac "nothing physically kept" guarantee for the
     * OFF state, and, just as importantly, frees the device's tight max-users cap (1 system + only 3 secondary
     * here): leftover non-ephemeral users otherwise pile up and break sign-in (createAndManageUser fails).
     *
     * A fresh boot never has a LIVE roamed session — the gate is the boot state and the .roamsession marker
     * lives in the power-off-cleared tmpfs — so removing every device-owner-managed secondary user here is
     * always correct and can't race a login (none has happened yet). The store still holds each profile's
     * last-sealed state, so the next login restores it; only post-last-sync changes from an unclean shutdown
     * are lost, which is the amnesiac contract.
     */
    private void wipeLeftoverRoamedUsers(Context context) {
        try {
            android.app.admin.DevicePolicyManager dpm = (android.app.admin.DevicePolicyManager)
                    context.getSystemService(Context.DEVICE_POLICY_SERVICE);
            if (dpm == null || !dpm.isDeviceOwnerApp(context.getPackageName())) return;
            android.content.ComponentName admin = new android.content.ComponentName(context, AdminReceiver.class);
            for (android.os.UserHandle uh : dpm.getSecondaryUsers(admin)) {
                int uid = uh.getIdentifier();
                boolean ok = dpm.removeUser(admin, uh);
                android.util.Log.i("NowhereChooser", "boot-wipe: removeUser " + uid + " ok=" + ok);
                // removeUser leaves /data/system_ce/<uid> behind; rm it so a reused uid can't inherit the stale,
                // key-mismatched CE and crash system_server on unlock (DIA-20260625-13).
                ChooserActivity.cleanUserStorage(uid);
            }
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "boot-wipe", t);
        }
    }
}
