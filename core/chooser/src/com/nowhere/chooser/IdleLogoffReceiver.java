package com.nowhere.chooser;

import android.app.KeyguardManager;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.net.LocalSocket;
import android.net.LocalSocketAddress;
import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.util.concurrent.atomic.AtomicBoolean;

/**
 * Idle-timeout auto-lock (P3/P4, docs/resumable-session.md). The user-0 gate arms an AlarmManager alarm on
 * screen-off (cancelled on unlock); if it fires, the roamed session has been LOCKED + IDLE for the timeout.
 * In the resumable-cold model that idle action is a COLD-LOCK, not a wipe: stop the user so its CE key is
 * evicted (ciphertext at rest, resumable with the passphrase), keeping the data on the device. The keyguard
 * alone leaves CE decrypted while merely locked (lock-model.md L3), so this timeout -> cold-lock is the real
 * at-rest guarantee for an unattended locked phone, while preserving the session (no re-download on resume).
 * The longer 12 h hard bound that escalates a still-cold-locked, un-resumed session to a full amnesiac WIPE
 * is a separate alarm armed AT cold-lock (ColdWipeReceiver / armWipeWatcher, DIA-20260626-03); this is the
 * 15-min default cold-lock that fires first.
 *
 * This runs in USER 0 (the alarm's user) and must NOT depend on the locked roamed UI: launching cross-user
 * into a locked, screen-off user is DEFERRED by Android until unlock (so an attacker who never unlocks would
 * never trigger it). Instead it drives the lock HEADLESSLY -- the daemon's COLD-LOCK reads the *marked* roam
 * session (readRoamSession, not the connecting user) and queues the switch+stop for the user-0 reap watcher.
 * Not exported; only the gate's own alarm PendingIntent reaches it.
 */
public class IdleLogoffReceiver extends BroadcastReceiver {
    private static final String SOCKET = "nowhere_login";
    // Hard cap on the COLD-LOCK round-trip, WELL under the 60s background-broadcast ANR limit. LocalSocket has
    // NO connect timeout, so a wedged/offline daemon (accept loop stuck in a store retry) blocks connect()
    // indefinitely; goAsync() then holds the broadcast pending until 60s, when Android ANRs and KILLS the
    // persistent gate process (proven on FP3 2026-07-01 -- the "black screen" gate-kill cascade, #77). The
    // cold-lock is best-effort (the alarm re-fires on the next screen-off), so we bound the whole round-trip
    // and finish the broadcast no matter what -- the gate is never taken down by an unresponsive daemon.
    private static final int CONNECT_CAP_MS = 8000; // watchdog: covers the unbounded connect()
    private static final int READ_CAP_MS = 5000;    // socket read timeout (daemon replies at once)

    @Override
    public void onReceive(Context ctx, Intent intent) {
        try {
            KeyguardManager km = (KeyguardManager) ctx.getSystemService(Context.KEYGUARD_SERVICE);
            // Belt-and-suspenders to the USER_PRESENT cancel: if the user unlocked since the alarm was armed,
            // the keyguard is gone -> they're active, so do NOT lock them out.
            boolean locked = km == null || km.isKeyguardLocked();
            boolean live = ChooserActivity.isSessionLive();
            android.util.Log.i("NowhereChooser", "idle cold-lock alarm live=" + live + " locked=" + locked);
            if (!live || !locked) return;

            final PendingResult pr = goAsync();     // keep the receiver alive for the (quick) socket round-trip
            final LocalSocket s = new LocalSocket();
            final AtomicBoolean done = new AtomicBoolean(false);
            // Finish the broadcast EXACTLY once, whichever of worker/watchdog gets there first.
            final Runnable finish = () -> { if (done.compareAndSet(false, true)) pr.finish(); };

            Thread worker = new Thread(() -> {
                try {
                    s.connect(new LocalSocketAddress(SOCKET, LocalSocketAddress.Namespace.RESERVED));
                    s.setSoTimeout(READ_CAP_MS); // the daemon replies at once; the switch+stop runs via the reap watcher
                    OutputStream os = s.getOutputStream();
                    os.write("COLD-LOCK\n".getBytes("UTF-8"));
                    os.flush();
                    BufferedReader br = new BufferedReader(new InputStreamReader(s.getInputStream(), "UTF-8"));
                    String reply = br.readLine();
                    android.util.Log.i("NowhereChooser", "idle COLD-LOCK reply=" + reply);
                } catch (Throwable t) {
                    android.util.Log.w("NowhereChooser", "idle COLD-LOCK", t);
                } finally {
                    try { s.close(); } catch (Throwable ignore) {}
                    finish.run();
                }
            });
            worker.setDaemon(true);
            worker.start();

            // Watchdog: connect() has no timeout, so if the daemon is wedged the worker never returns. At the
            // cap, close the socket (unblocks the worker's blocking connect/read) and finish the broadcast
            // anyway -- so it can NEVER reach the 60s ANR and kill the gate. The alarm retries next screen-off.
            Thread watchdog = new Thread(() -> {
                try { Thread.sleep(CONNECT_CAP_MS); } catch (InterruptedException ignore) {}
                if (!done.get()) {
                    android.util.Log.w("NowhereChooser", "idle COLD-LOCK: daemon unresponsive, abandoning (will retry)");
                    try { s.close(); } catch (Throwable ignore) {}
                    finish.run();
                }
            });
            watchdog.setDaemon(true);
            watchdog.start();
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "idle cold-lock", t);
        }
    }
}
