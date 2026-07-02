package com.nowhere.chooser;

import android.net.LocalSocket;
import android.net.LocalSocketAddress;
import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.atomic.AtomicBoolean;

/**
 * Bounded round-trips to the login daemon's LocalSocket (#77). {@link LocalSocket} has NO connect timeout
 * ({@code setSoTimeout} is the READ timeout only), so a wedged daemon -- its accept loop stuck in a store
 * retry -- blocks {@code connect()} FOREVER. On the idle/wipe alarm that reached the 60s background-broadcast
 * ANR and KILLED the persistent gate process (bounded separately in {@link IdleLogoffReceiver}); on every other
 * path it silently hung the caller's worker thread: a stalled reap poll (a queued logoff never reaps), a skipped
 * 12h hard-wipe (a security backstop that never fires), or a UI action that spins with no error.
 *
 * {@link #connect} bounds the connect with a watchdog that closes the socket at the cap -- closing an in-flight
 * LocalSocket unblocks a stuck {@code connect()} (proven on FP3, #77) -- so no daemon call can hang forever. A
 * wedged daemon yields a "" reply / empty list and the caller retries. All callers already run off the main
 * thread, so the bounded blocking here is safe.
 */
final class DaemonSocket {
    static final String SOCKET = "nowhere_login";
    static final int CONNECT_CAP_MS = 8000; // healthy connect is instant; this only bites a wedged daemon

    private DaemonSocket() {}

    /** Connect with a bounded connect. Returns a connected socket (the caller sets its own read timeout and does
     *  its own I/O), or null if the daemon did not accept within {@link #CONNECT_CAP_MS}. */
    static LocalSocket connect() {
        final LocalSocket s = new LocalSocket();
        final AtomicBoolean settled = new AtomicBoolean(false); // whoever CAS-wins decides the socket's fate
        Thread wd = new Thread(() -> {
            try { Thread.sleep(CONNECT_CAP_MS); } catch (InterruptedException ignore) {}
            if (settled.compareAndSet(false, true)) { // watchdog won -> connect still pending -> unblock it
                try { s.close(); } catch (Throwable ignore) {}
            }
        });
        wd.setDaemon(true);
        wd.start();
        try {
            s.connect(new LocalSocketAddress(SOCKET, LocalSocketAddress.Namespace.RESERVED));
            if (!settled.compareAndSet(false, true)) { // watchdog already fired + closed the socket -> treat as fail
                try { s.close(); } catch (Throwable ignore) {}
                return null;
            }
            wd.interrupt(); // connect returned in time -> stop the (sleeping) watchdog
            return s;
        } catch (Throwable t) {
            settled.set(true);
            try { s.close(); } catch (Throwable ignore) {}
            return null;
        }
    }

    /** Bounded request -> first reply line ("" on any error or a wedged daemon). {@code readCapMs} is the read
     *  timeout for the reply (per caller: a local probe is quick; a store-touching command needs longer). */
    static String roundTrip(String msg, int readCapMs) {
        LocalSocket s = connect();
        if (s == null) return "";
        try {
            s.setSoTimeout(readCapMs);
            OutputStream os = s.getOutputStream();
            os.write(msg.getBytes("UTF-8"));
            os.flush();
            BufferedReader br = new BufferedReader(new InputStreamReader(s.getInputStream(), "UTF-8"));
            String line = br.readLine();
            return line == null ? "" : line.trim();
        } catch (Throwable t) {
            return "";
        } finally {
            try { s.close(); } catch (Throwable ignore) {}
        }
    }

    /** Bounded request -> ALL non-empty reply lines (the daemon writes the payload then closes). Empty on error. */
    static List<String> roundTripLines(String msg, int readCapMs) {
        ArrayList<String> lines = new ArrayList<>();
        LocalSocket s = connect();
        if (s == null) return lines;
        try {
            s.setSoTimeout(readCapMs);
            OutputStream os = s.getOutputStream();
            os.write(msg.getBytes("UTF-8"));
            os.flush();
            BufferedReader br = new BufferedReader(new InputStreamReader(s.getInputStream(), "UTF-8"));
            String line;
            while ((line = br.readLine()) != null) { line = line.trim(); if (!line.isEmpty()) lines.add(line); }
        } catch (Throwable t) {
            // return whatever lines arrived before the error
        } finally {
            try { s.close(); } catch (Throwable ignore) {}
        }
        return lines;
    }
}
