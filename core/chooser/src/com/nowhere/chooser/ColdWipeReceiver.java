package com.nowhere.chooser;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;

/**
 * #3 hard-wipe backstop (DIA-20260626-03, docs/lock-model.md). The user-0 gate arms an AlarmManager alarm at
 * COLD-LOCK (armWipeWatcher); cancelled when a session goes live again. If it fires, a cold-locked session has
 * been left un-resumed past the hard bound (default 12 h), so escalate it to a full amnesiac WIPE -- the same
 * removal a power-off boot-wipe would do, just on a timer. ChooserActivity.coldWipe re-checks the daemon's
 * .coldlock marker (so a resumed session is never wiped) before removing the user.
 *
 * Runs in USER 0 (the alarm's user); not exported -- only the gate's own alarm PendingIntent reaches it.
 */
public class ColdWipeReceiver extends BroadcastReceiver {
    @Override
    public void onReceive(Context ctx, Intent intent) {
        int uid = intent != null ? intent.getIntExtra("uid", -1) : -1;
        android.util.Log.i("NowhereChooser", "hard-wipe alarm uid=" + uid);
        if (uid >= 10) ChooserActivity.coldWipe(ctx, uid); // guard: only ever a secondary (roamed) user
    }
}
