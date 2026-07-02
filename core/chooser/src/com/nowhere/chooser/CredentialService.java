package com.nowhere.chooser;

import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.os.IBinder;

/**
 * L1 (lock-model.md): set the SESSION keyguard credential on the roamed (ephemeral) user.
 *
 * resetPasswordWithToken IGNORES a cross-user context -- invoked from user 0 it locks user 0 (the gate),
 * not the session (verified on the FP3 2026-06-23: createContextAsUser(uid) still set user 0's credential).
 * So the user-0 gate starts THIS service via startServiceAsUser(uid); it runs in the roamed user's OWN
 * process, where the chooser is the profile owner of that user. Here it enrolls a reset-password token
 * (which activates immediately with NO on-screen confirm -- the fresh ephemeral user has no prior
 * credential) and sets the credential, then stops. The passphrase rides in the Intent extra
 * (binder / in-memory) -- never on disk, never in any argv.
 */
public class CredentialService extends Service {
    @Override public IBinder onBind(Intent i) { return null; }

    @Override public int onStartCommand(Intent intent, int flags, int startId) {
        if (intent != null) {
            String pw = intent.getStringExtra("pw");
            if (pw != null && !pw.isEmpty()) setCredential(pw);
        }
        stopSelf();
        return START_NOT_STICKY;
    }

    private void setCredential(String pw) {
        try {
            android.app.admin.DevicePolicyManager dpm =
                    (android.app.admin.DevicePolicyManager) getSystemService(Context.DEVICE_POLICY_SERVICE);
            android.content.ComponentName admin = new android.content.ComponentName(this, AdminReceiver.class);
            byte[] token = new byte[32];
            new java.security.SecureRandom().nextBytes(token);
            boolean tokenSet = dpm.setResetPasswordToken(admin, token);
            boolean active = tokenSet && dpm.isResetPasswordTokenActive(admin);
            boolean pwSet = false;
            if (active) {
                // flags=0: set the credential but DON'T force an immediate lock. RESET_PASSWORD_REQUIRE_ENTRY
                // slam-locks the session the instant we set it, so the user (who just typed the passphrase at
                // the gate to log in) is dumped onto a lockscreen and must type it AGAIN before reaching home.
                // With no flag the session stays unlocked after login; the secure keyguard still challenges on
                // the next screen-off. (DIA-20260623-10)
                pwSet = dpm.resetPasswordWithToken(admin, pw, token, 0);
            }
            android.util.Log.i("NowhereChooser", "L1 CredentialService user="
                    + android.os.Process.myUserHandle() + " tokenSet=" + tokenSet
                    + " active=" + active + " pwSet=" + pwSet);
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "CredentialService.setCredential", t);
        }
    }
}
