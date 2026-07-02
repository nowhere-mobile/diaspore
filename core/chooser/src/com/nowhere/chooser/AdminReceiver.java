package com.nowhere.chooser;

import android.app.admin.DeviceAdminReceiver;
import android.app.admin.DevicePolicyManager;
import android.content.Context;
import android.content.Intent;
import android.os.UserHandle;

/**
 * Device-admin receiver. Lets the chooser be the DEVICE OWNER (kiosk / Lock Task) and PROFILE OWNER of each
 * roamed user. It also carries the L4 failed-attempt backstop: too many wrong unlocks of a roamed session
 * auto-logs-off (seal + reap -> blank gate), bounding a brute-force of the session keyguard.
 */
public class AdminReceiver extends DeviceAdminReceiver {
    // Wrong unlocks of a roamed session before auto-logoff. NOT setMaximumFailedPasswordsForWipe (that
    // factory-resets the device); we keep the device's provisioning/store config and just revert to the
    // amnesiac gate.
    private static final int MAX_FAILED = 5;

    @Override
    public void onPasswordFailed(Context context, Intent intent, UserHandle user) {
        try {
            DevicePolicyManager dpm =
                    (DevicePolicyManager) context.getSystemService(Context.DEVICE_POLICY_SERVICE);
            int fails = dpm.getCurrentFailedPasswordAttempts();
            android.util.Log.i("NowhereChooser", "L4 onPasswordFailed attempts=" + fails + " user=" + user);
            if (fails >= MAX_FAILED) {
                context.startActivity(new Intent(context, ProfileActivity.class)
                        .putExtra("auto", true)
                        .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK));
            }
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "L4 onPasswordFailed", t);
        }
    }
}
