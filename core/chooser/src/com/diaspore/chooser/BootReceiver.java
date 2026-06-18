package com.diaspore.chooser;

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
            Intent i = new Intent(context, ChooserActivity.class);
            // NO_ANIMATION + bring-to-front: come up immediately over launcher3 (which the system shows at
            // boot_completed) with no slide-in, minimizing the ~1-2s home-flash before the gate appears.
            i.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK
                    | Intent.FLAG_ACTIVITY_NO_ANIMATION
                    | Intent.FLAG_ACTIVITY_REORDER_TO_FRONT);
            context.startActivity(i);
        }
    }
}
