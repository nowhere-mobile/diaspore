package com.nowhere.chooser;

import android.Manifest;
import android.app.Activity;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.database.Cursor;
import android.graphics.drawable.GradientDrawable;
import android.net.LocalSocket;
import android.net.LocalSocketAddress;
import android.net.Uri;
import android.os.Bundle;
import android.os.UserManager;
import android.provider.CalendarContract;
import android.provider.ContactsContract;
import android.util.Log;
import android.util.TypedValue;
import android.view.Gravity;
import android.view.View;
import android.widget.Button;
import android.widget.EditText;
import android.widget.LinearLayout;
import android.widget.TextView;
import java.io.BufferedReader;
import java.io.ByteArrayOutputStream;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStream;

/**
 * The post-login Profile app (the dock icon "Profile"; was LogoffActivity until the regroup). It NAMES the
 * signed-in profile -- the user's name IS the Android user name (set at login from the profile name), so
 * getUserName() surfaces "who am I" with no extra plumbing -- and hosts the session's actions + settings:
 * Storage & subscription, Lock / Log off, security/auto-lock, preferences, delete.
 *
 * "Log off" is one of those actions: it tells the root login daemon (LOGOUT) to seal the live session to the
 * store, then reap this profile IN PLACE -- switch back to the gate on user 0 and stop+wipe this ephemeral
 * user (its /data is deleted and the FBE key destroyed) -- all WITHOUT a reboot, so logout takes seconds. If
 * the daemon is unreachable it falls back to a reboot (which also wipes the ephemeral user). The seal can
 * touch the network, so it runs off the UI thread. Cancel just returns to the home.
 */
public class ProfileActivity extends Activity {
    private static final String SOCKET = "nowhere_login";

    // Set once logoff begins: the "Logging off…" screen is then UNESCAPABLE until the reap switches to the
    // gate -- you must not be able to Home/Back your way back into a session you've already logged off.
    private boolean loggingOff = false;

    private static final int MATCH = LinearLayout.LayoutParams.MATCH_PARENT;
    private static final int WRAP  = LinearLayout.LayoutParams.WRAP_CONTENT;

    private static final int BG        = 0xFF0B0F14;
    private static final int INK       = 0xFFEAF1EE;
    private static final int MUTED     = 0xFF69788A;
    private static final int ACCENT    = 0xFF4FD6AC;
    private static final int ON_ACCENT = 0xFF05281F;
    private static final int FIELD_BG  = 0xFF121922;
    private static final int FIELD_LN  = 0xFF232E39;
    private static final int HINT      = 0xFF8595A4;
    private static final int FIELD_TX  = 0xFFE6EDF3;

    // #4 (DIA-20260626-04): "LOCAL" = a throwaway (logged in with a name that has no stored account) -> offer
    // "Save to your store"; "STORED" = a real roaming profile; "" = unknown until GET-SESSION-TYPE returns.
    private volatile String sessionType = "";

    // DIA-20260628-09 P2 (export): the Your-data status line (shared by Back up now + Export a copy) and the
    // runtime-permission request code for the contacts/calendar read the export needs.
    private static final int REQ_EXPORT = 0x5870;
    // Export reads these on demand -- whatever is granted gets exported. Declared in the manifest, NOT
    // pre-granted (the kiosk never holds standing access). P2 = contacts/calendar; P4 = SMS + call log.
    private static final String[] EXPORT_PERMS = {
            Manifest.permission.READ_CONTACTS, Manifest.permission.READ_CALENDAR,
            Manifest.permission.READ_SMS, Manifest.permission.READ_CALL_LOG,
    };
    private TextView dataStatusTv;
    private android.widget.ProgressBar dataBar; // #88: shared Back up / Export progress bar (determinate for backup, indeterminate for export)

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        // L4: an auto-logoff backstop (idle timeout / too many failed unlocks) launches this with auto=true ->
        // skip the confirmation and seal+reap immediately. Otherwise this is the user-driven Profile screen.
        if (getIntent() != null && getIntent().getBooleanExtra("auto", false)) {
            doLogoff();
            return;
        }
        // #4: learn whether this session is a throwaway BEFORE drawing the menu, so it shows the right warning +
        // the "Save to your store" button. Fast local-socket query; the brief pre-draw wait is imperceptible.
        new Thread(() -> {
            final String t = sendDaemon("GET-SESSION-TYPE\n").trim();
            runOnUiThread(() -> { sessionType = t; showConfirm(); });
        }).start();
    }

    /** The current (roamed) user's name == the profile name (createRoamUser set it). null/blank -> generic. */
    private String profileName() {
        try {
            String n = ((UserManager) getSystemService(USER_SERVICE)).getUserName();
            if (n != null && !n.trim().isEmpty()) return n.trim();
        } catch (Throwable ignore) {
        }
        return null;
    }

    private void showConfirm() {
        final String name = profileName();
        final boolean local = "LOCAL".equals(sessionType); // #4: a throwaway -> not saved anywhere yet

        LinearLayout ll = new LinearLayout(this);
        ll.setOrientation(LinearLayout.VERTICAL);
        ll.setBackgroundColor(BG);
        ll.setPadding(dp(22), dp(44), dp(22), dp(30));

        // --- identity header (centered): name + a one-line status ---
        TextView nameTv = new TextView(this);
        nameTv.setText(name == null ? "Signed in" : name);
        nameTv.setTextSize(TypedValue.COMPLEX_UNIT_SP, 30);
        nameTv.setTextColor(INK);
        nameTv.setGravity(Gravity.CENTER);
        nameTv.setLetterSpacing(0.02f);
        ll.addView(nameTv, new LinearLayout.LayoutParams(MATCH, WRAP));

        TextView status = new TextView(this);
        status.setText(local ? "Throwaway — lives only on this phone" : "Signed in · roams to your store");
        status.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        status.setTextColor(MUTED);
        status.setGravity(Gravity.CENTER);
        LinearLayout.LayoutParams stlp = new LinearLayout.LayoutParams(MATCH, WRAP);
        stlp.topMargin = dp(5);
        stlp.bottomMargin = dp(26);
        ll.addView(status, stlp);

        // --- throwaway: the prominent Save path + a short note ---
        if (local) {
            ll.addView(accentButton("Save to your store", () -> doPromote()), btnLp(0));
            TextView note = new TextView(this);
            note.setText("Saving keeps this profile and lets it roam to other devices. Otherwise it's erased "
                    + "on log off or power off.");
            note.setTextSize(TypedValue.COMPLEX_UNIT_SP, 12);
            note.setTextColor(MUTED);
            note.setGravity(Gravity.CENTER);
            note.setLineSpacing(dp(3), 1f);
            LinearLayout.LayoutParams nlp = new LinearLayout.LayoutParams(MATCH, WRAP);
            nlp.topMargin = dp(10);
            ll.addView(note, nlp);
        }

        // --- session actions: Lock (stored, the everyday "stepping away") then Log off ---
        if (name != null && !local) {
            ll.addView(accentButton("Lock — keep on this device", () -> doColdLock()), btnLp(0));
            ll.addView(secondaryButton("Log off " + name, () -> doLogoff()), btnLp(dp(10)));
        } else if (local) {
            ll.addView(secondaryButton(name == null ? "Log off" : "Log off " + name, () -> doLogoff()), btnLp(dp(16)));
        } else {
            ll.addView(accentButton("Log off", () -> doLogoff()), btnLp(0));
        }

        // --- the menu: one row per area, each opening its OWN screen (DIA-20260628-08). The hub used to inline
        // every setting as grouped cards -- too crowded; now it's just a table of contents that points onward. ---
        if (name != null && !local) {
            sectionCard(ll, null,
                    new String[]{"Storage & subscription", "Security", "Preferences", "Your data"},
                    new Runnable[]{() -> showStorage(), () -> showSecurity(), () -> showPreferences(),
                            () -> showYourData()}, false);
        } else if (name != null) { // a throwaway: device-local Security (idle/wipe) + Preferences; no store/delete
            sectionCard(ll, null,
                    new String[]{"Security", "Preferences"},
                    new Runnable[]{() -> showSecurity(), () -> showPreferences()}, false);
        }

        // Delete: a deliberate, separate destructive action (stored profiles only) -- a quiet centered link, NOT a
        // menu row, so it doesn't read as "just another setting". Sits above Done, off the easiest-to-tap spot.
        if (name != null && !local) {
            TextView del = new TextView(this);
            del.setText("Delete this profile");
            del.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
            del.setTextColor(0xFFE05B5B);
            del.setGravity(Gravity.CENTER);
            del.setPadding(dp(8), dp(30), dp(8), dp(8));
            ll.addView(del, new LinearLayout.LayoutParams(MATCH, WRAP));
            del.setOnClickListener(v -> showDeleteConfirm(name));
        }

        // Done: leave the Profile app, back to the home screen. (Was "Cancel" -- on a menu there's nothing to
        // cancel; the sub-screens use "Back" to return HERE, the menu uses "Done" to return home.)
        TextView done = new TextView(this);
        done.setText("Done");
        done.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
        done.setTextColor(MUTED);
        done.setGravity(Gravity.CENTER);
        done.setPadding(dp(8), dp(18), dp(8), dp(8));
        ll.addView(done, new LinearLayout.LayoutParams(MATCH, WRAP));
        done.setOnClickListener(v -> finish());

        android.widget.ScrollView sc = new android.widget.ScrollView(this);
        sc.setBackgroundColor(BG);
        sc.addView(ll);
        setContentView(sc);
    }

    /** Full-width primary (accent) button with its click. */
    private Button accentButton(String text, final Runnable onClick) {
        Button b = accentBtn(text); // reuse the accent-styled builder
        b.setOnClickListener(v -> onClick.run());
        return b;
    }

    /** Full-width secondary (outlined) button -- for "Log off" when Lock is the primary action. */
    private Button secondaryButton(String text, final Runnable onClick) {
        Button b = new Button(this);
        b.setText(text);
        b.setAllCaps(false);
        b.setTextSize(TypedValue.COMPLEX_UNIT_SP, 15);
        b.setTextColor(ACCENT);
        GradientDrawable bg = new GradientDrawable();
        bg.setColor(BG);
        bg.setCornerRadius(dp(13));
        bg.setStroke(dp(1), FIELD_LN);
        b.setBackground(bg);
        b.setStateListAnimator(null);
        b.setSoundEffectsEnabled(false);
        b.setPadding(0, dp(13), 0, dp(13));
        b.setOnClickListener(v -> onClick.run());
        return b;
    }

    private LinearLayout.LayoutParams btnLp(int topMargin) {
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(MATCH, WRAP);
        lp.topMargin = topMargin;
        return lp;
    }

    /** A titled card of tappable rows (label left, chevron right) with hairline dividers. danger -> red, no chevron. */
    private void sectionCard(LinearLayout parent, String header, String[] labels, Runnable[] actions, boolean danger) {
        if (header != null) {
            TextView h = new TextView(this);
            h.setText(header.toUpperCase(java.util.Locale.US));
            h.setTextSize(TypedValue.COMPLEX_UNIT_SP, 11);
            h.setTextColor(MUTED);
            h.setLetterSpacing(0.08f);
            h.setPadding(dp(8), dp(26), dp(8), dp(8));
            parent.addView(h, new LinearLayout.LayoutParams(MATCH, WRAP));
        } else {
            android.view.View sp = new android.view.View(this);
            parent.addView(sp, new LinearLayout.LayoutParams(MATCH, dp(20)));
        }
        LinearLayout card = new LinearLayout(this);
        card.setOrientation(LinearLayout.VERTICAL);
        GradientDrawable cbg = new GradientDrawable();
        cbg.setColor(FIELD_BG);
        cbg.setCornerRadius(dp(14));
        cbg.setStroke(dp(1), FIELD_LN);
        card.setBackground(cbg);
        parent.addView(card, new LinearLayout.LayoutParams(MATCH, WRAP));
        for (int i = 0; i < labels.length; i++) {
            if (i > 0) {
                android.view.View div = new android.view.View(this);
                div.setBackgroundColor(FIELD_LN);
                LinearLayout.LayoutParams dlp = new LinearLayout.LayoutParams(MATCH, Math.max(1, dp(0.5f)));
                dlp.leftMargin = dp(16);
                card.addView(div, dlp);
            }
            card.addView(cardRow(labels[i], actions[i], danger));
        }
    }

    private LinearLayout cardRow(String label, final Runnable onClick, boolean danger) {
        LinearLayout row = new LinearLayout(this);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.setGravity(Gravity.CENTER_VERTICAL);
        row.setPadding(dp(16), dp(15), dp(16), dp(15));
        TextView l = new TextView(this);
        l.setText(label);
        l.setTextSize(TypedValue.COMPLEX_UNIT_SP, 15);
        l.setTextColor(danger ? 0xFFE05B5B : INK);
        row.addView(l, new LinearLayout.LayoutParams(0, WRAP, 1f));
        if (!danger) {
            TextView chev = new TextView(this);
            chev.setText("›"); // a right chevron
            chev.setTextSize(TypedValue.COMPLEX_UNIT_SP, 18);
            chev.setTextColor(MUTED);
            row.addView(chev, new LinearLayout.LayoutParams(WRAP, WRAP));
        }
        row.setOnClickListener(v -> onClick.run());
        return row;
    }

    /** A titled sub-screen header (title + one muted line) on the standard menu background; callers add the card(s)
     *  then a Back link. Factors the boilerplate shared by the menu's child screens (DIA-20260628-08). */
    private LinearLayout menuScreen(String title, String subtitle) {
        LinearLayout ll = new LinearLayout(this);
        ll.setOrientation(LinearLayout.VERTICAL);
        ll.setBackgroundColor(BG);
        ll.setPadding(dp(22), dp(44), dp(22), dp(30));

        TextView t = new TextView(this);
        t.setText(title);
        t.setTextColor(INK);
        t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 24);
        t.setGravity(Gravity.CENTER);
        ll.addView(t, new LinearLayout.LayoutParams(MATCH, WRAP));

        TextView sub = new TextView(this);
        sub.setText(subtitle);
        sub.setTextColor(MUTED);
        sub.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        sub.setGravity(Gravity.CENTER);
        LinearLayout.LayoutParams slp = new LinearLayout.LayoutParams(MATCH, WRAP);
        slp.topMargin = dp(5);
        slp.bottomMargin = dp(2);
        ll.addView(sub, slp);
        return ll;
    }

    /** Add a centered "Back" link (returns to the Profile menu) and present the screen in a ScrollView. */
    private void finishMenuScreen(LinearLayout ll) {
        TextView back = new TextView(this);
        back.setText("Back");
        back.setTextColor(ACCENT);
        back.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
        back.setGravity(Gravity.CENTER);
        back.setPadding(dp(8), dp(22), dp(8), dp(8));
        ll.addView(back, new LinearLayout.LayoutParams(MATCH, WRAP));
        back.setOnClickListener(v -> showConfirm());

        android.widget.ScrollView sc = new android.widget.ScrollView(this);
        sc.setBackgroundColor(BG);
        sc.addView(ll);
        setContentView(sc);
    }

    /** Security menu (DIA-20260628-08): the passphrase + the two auto-lock backstops, each its own screen/dialog.
     *  Change passphrase is store-only -- a throwaway has no stored passphrase to change, so it's hidden there. */
    private void showSecurity() {
        final String name = profileName();
        final boolean local = "LOCAL".equals(sessionType);
        LinearLayout ll = menuScreen("Security", "how this profile locks and unlocks");

        java.util.ArrayList<String> labels = new java.util.ArrayList<>();
        final java.util.ArrayList<Runnable> actions = new java.util.ArrayList<>();
        if (!local) { labels.add("Change passphrase"); actions.add(() -> showChangePass(name)); }
        labels.add("Sign out after inactivity");   actions.add(() -> showIdlePicker());
        labels.add("Auto-wipe when left locked");  actions.add(() -> showWipePicker());
        sectionCard(ll, null, labels.toArray(new String[0]), actions.toArray(new Runnable[0]), false);

        finishMenuScreen(ll);
    }

    /** Preferences menu (DIA-20260628-08): how the profile behaves. One item today (time zone); a real screen so
     *  locale/units/etc. are a one-line add later. */
    private void showPreferences() {
        LinearLayout ll = menuScreen("Preferences", "how this profile behaves");
        sectionCard(ll, null, new String[]{"Time zone"}, new Runnable[]{() -> showTimezonePicker()}, false);
        finishMenuScreen(ll);
    }

    /** Your data (DIA-20260628-09): it's yours -- back it up, and take a portable copy elsewhere.
     *  - Back up now (P1): forces an immediate seal of the live session to the store via the daemon (BACKUP),
     *    reusing the logoff seal-progress channel for a "Backing up… N%" line. The data already roams (~every
     *    minute); this just captures the latest changes on demand.
     *  - Export a copy (P2, tier-1): writes your contacts (vCard) + calendar (iCal) as standard files into your
     *    Downloads folder, so you can take them to any phone. Reads via the providers under a runtime permission
     *    you grant on demand (a privacy OS shouldn't standing-grant the kiosk your contacts). Media + a portable
     *    archive are the next increments; see docs/data-export.md. */
    private void showYourData() {
        LinearLayout ll = menuScreen("Your data", "it's yours — back it up, or take it with you");

        TextView note = new TextView(this);
        note.setText("Saved to your store automatically, about once a minute. Back up to capture your latest "
                + "changes now, or export a portable copy to take elsewhere.");
        note.setTextColor(MUTED);
        note.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        note.setGravity(Gravity.CENTER);
        note.setLineSpacing(dp(4), 1f);
        LinearLayout.LayoutParams nlp = new LinearLayout.LayoutParams(MATCH, WRAP);
        nlp.topMargin = dp(24);
        nlp.bottomMargin = dp(22);
        ll.addView(note, nlp);

        ll.addView(accentButton("Back up now", () -> doBackup()), btnLp(0));
        ll.addView(secondaryButton("Export a copy", () -> startExport()), btnLp(dp(10)));
        ll.addView(secondaryButton("Restore a snapshot", () -> showSnapshots()), btnLp(dp(10))); // #58

        dataStatusTv = new TextView(this); // shared status line for both actions
        dataStatusTv.setTextColor(MUTED);
        dataStatusTv.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        dataStatusTv.setGravity(Gravity.CENTER);
        dataStatusTv.setLineSpacing(dp(4), 1f);
        LinearLayout.LayoutParams dlp = new LinearLayout.LayoutParams(MATCH, WRAP);
        dlp.topMargin = dp(20);
        ll.addView(dataStatusTv, dlp);

        // #88: a visual progress bar under the status line -- determinate %-driven for "Back up now" (from
        // SEAL-STATUS), indeterminate for "Export a copy" (a single-blob upload with no % yet). Hidden when idle.
        dataBar = new android.widget.ProgressBar(this, null, android.R.attr.progressBarStyleHorizontal);
        dataBar.setMax(100);
        dataBar.setVisibility(View.GONE);
        dataBar.setProgressTintList(android.content.res.ColorStateList.valueOf(ACCENT));
        dataBar.setIndeterminateTintList(android.content.res.ColorStateList.valueOf(ACCENT));
        LinearLayout.LayoutParams barLp = new LinearLayout.LayoutParams(MATCH, WRAP);
        barLp.topMargin = dp(14);
        ll.addView(dataBar, barLp);

        finishMenuScreen(ll);
    }

    // #88 progress-bar helpers -- all post to the UI thread and no-op if the Your-data screen has been left
    // (dataBar cleared). Determinate for a known %, indeterminate for "working, no ETA", hide when idle/done.
    private void dataBarDeterminate(final int pct) {
        runOnUiThread(() -> { if (dataBar == null) return;
            dataBar.setIndeterminate(false); dataBar.setProgress(Math.max(0, Math.min(100, pct)));
            dataBar.setVisibility(View.VISIBLE); });
    }
    private void dataBarIndeterminate() {
        runOnUiThread(() -> { if (dataBar == null) return;
            dataBar.setIndeterminate(true); dataBar.setVisibility(View.VISIBLE); });
    }
    private void dataBarHide() {
        runOnUiThread(() -> { if (dataBar == null) return;
            dataBar.setIndeterminate(false); dataBar.setVisibility(View.GONE); });
    }
    /** #88: update the shared status line to the current export phase, from a worker thread. */
    private void runPhase(final TextView st, final String text) {
        runOnUiThread(() -> { st.setTextColor(ACCENT); st.setText(text); });
    }

    /** "Back up now": force an immediate seal to the store + poll SEAL-STATUS for a progress line + bar (#88). */
    private void doBackup() {
        final TextView st = dataStatusTv;
        st.setTextColor(ACCENT);
        st.setText("Backing up…");
        dataBarIndeterminate(); // #88: show activity immediately; flips to determinate once the seal reports a %
        new Thread(() -> {
            final String r = sendDaemon("BACKUP\n").trim();
            if (r.startsWith("OK")) {
                while (true) {
                    String s = sealStatus();
                    if (s == null || s.startsWith("DONE")) break;
                    if (s.startsWith("SEALING") && sealPct(s) > 0) { // only %-bearing frames; the initial "Backing up…" holds until then (no number-less flicker, DIA-20260701-03)
                        final int pct = sealPct(s);
                        dataBarDeterminate(pct); // #88
                        runOnUiThread(() -> st.setText("Backing up…  " + pct + "%"));
                    }
                    try { Thread.sleep(400); } catch (InterruptedException ie) { break; }
                }
                dataBarHide(); // #88
                runOnUiThread(() -> { st.setTextColor(ACCENT); st.setText("Backed up just now."); });
            } else {
                final String msg = r.startsWith("LOCAL")
                        ? "This profile is a throwaway — it lives only on this phone, so there's nothing to back up."
                        : r.startsWith("BUSY") ? "A backup is already running — give it a moment."
                        : r.startsWith("NONE") ? "No active session to back up."
                        : "Couldn't back up — check your connection and try again.";
                dataBarHide(); // #88
                runOnUiThread(() -> { st.setTextColor(MUTED); st.setText(msg); });
            }
        }).start();
    }

    /** "Export a copy": contacts (vCard) + calendar (iCal) + messages/calls (XML) -> one zip, sealed under the
     *  profile key into the store (web-retrievable with the passphrase). Reads via the providers under runtime
     *  permissions requested on demand; whatever you grant gets exported, the rest is silently skipped. */
    private void startExport() {
        for (String p : EXPORT_PERMS) {
            if (checkSelfPermission(p) != PackageManager.PERMISSION_GRANTED) {
                requestPermissions(EXPORT_PERMS, REQ_EXPORT); // request all the missing ones at once
                return;
            }
        }
        runExport();
    }

    @Override
    public void onRequestPermissionsResult(int req, String[] perms, int[] results) {
        super.onRequestPermissionsResult(req, perms, results);
        if (req == REQ_EXPORT) runExport(); // export whatever was granted; each builder tolerates a denied grant
    }

    /** Build the standard files (vCard + iCal), package them as one zip bundle, and hand it to the daemon to seal
     *  under the profile key + store at export/<name>. A roamed session is a secondary Android user with no usable
     *  shared storage / MTP, so the store (retrieved later from the web with your passphrase) is the destination. */
    private void runExport() {
        final TextView st = dataStatusTv;
        if (st == null) return;
        st.setTextColor(ACCENT);
        st.setText("Exporting…");
        // #88: an export runs BUILD (read each provider) -> PACKAGE -> UPLOAD. Each provider read can be slow for a
        // large history, so name the phase as we go; the bar is indeterminate because a single-blob upload has no %
        // (real % arrives with the chunked media export, #52). runPhase() updates the status line off the UI thread.
        dataBarIndeterminate();
        new Thread(() -> {
            final int[] cN = {0}, eN = {0}, sN = {0}, lN = {0};
            runPhase(st, "Reading contacts…");
            byte[] vcf = buildContactsVCard(cN);
            runPhase(st, "Reading calendar…");
            byte[] ics = buildCalendarICal(eN);
            runPhase(st, "Reading messages…");
            byte[] sms = buildSmsXml(sN);
            runPhase(st, "Reading calls…");
            byte[] calls = buildCallLogXml(lN);
            java.util.LinkedHashMap<String, byte[]> entries = new java.util.LinkedHashMap<>();
            if (vcf != null) entries.put("contacts.vcf", vcf);
            if (ics != null) entries.put("calendar.ics", ics);
            if (sms != null) entries.put("messages.xml", sms);
            if (calls != null) entries.put("calls.xml", calls);
            if (entries.isEmpty()) {
                dataBarHide(); // #88
                runOnUiThread(() -> { st.setTextColor(MUTED);
                        st.setText("Nothing to export yet, or no access was granted."); });
                return;
            }
            String manifest = "{\"version\":2,\"contacts\":" + cN[0] + ",\"events\":" + eN[0]
                    + ",\"messages\":" + sN[0] + ",\"calls\":" + lN[0] + ",\"created\":\"" + dateStamp() + "\"}\n";
            runPhase(st, "Packaging…");
            byte[] bundle;
            try {
                bundle = buildBundleZip(entries, manifest);
            } catch (Throwable t) {
                Log.w("NowhereChooser", "buildBundle", t);
                dataBarHide(); // #88
                runOnUiThread(() -> { st.setTextColor(MUTED); st.setText("Couldn't package the export — try again."); });
                return;
            }
            runPhase(st, "Uploading your copy…");
            final String r = sendExport(bundle);
            dataBarHide(); // #88
            runOnUiThread(() -> {
                if (r.startsWith("OK")) {
                    java.util.List<String> parts = new java.util.ArrayList<>();
                    if (cN[0] > 0) parts.add(cN[0] + (cN[0] == 1 ? " contact" : " contacts"));
                    if (eN[0] > 0) parts.add(eN[0] + (eN[0] == 1 ? " event" : " events"));
                    if (sN[0] > 0) parts.add(sN[0] + (sN[0] == 1 ? " message" : " messages"));
                    if (lN[0] > 0) parts.add(lN[0] + (lN[0] == 1 ? " call" : " calls"));
                    st.setTextColor(ACCENT);
                    st.setText("Exported " + android.text.TextUtils.join(", ", parts)
                            + " to your store. Download them from the nowhere website with your passphrase.");
                } else {
                    final String msg = r.startsWith("LOCAL")
                            ? "This profile is a throwaway — there's no store to export to."
                            : r.startsWith("NONE") ? "No active session to export."
                            : "Couldn't export — check your connection and try again.";
                    st.setTextColor(MUTED);
                    st.setText(msg);
                }
            });
        }).start();
    }

    /** Build every contact as one vCard via the provider's OWN multi-vCard stream (the path the Contacts app's
     *  export uses -- robust, no hand-rolled formatting). Returns the vCard bytes (null when none/error); count -> out[0]. */
    private byte[] buildContactsVCard(int[] out) {
        try {
            java.util.LinkedHashSet<String> keys = new java.util.LinkedHashSet<>();
            try (Cursor c = getContentResolver().query(ContactsContract.Contacts.CONTENT_URI,
                    new String[]{ ContactsContract.Contacts.LOOKUP_KEY }, null, null, null)) {
                if (c != null) while (c.moveToNext()) {
                    String k = c.getString(0);
                    if (k != null && !k.isEmpty()) keys.add(k);
                }
            }
            out[0] = keys.size();
            if (keys.isEmpty()) return null;
            Uri uri = Uri.withAppendedPath(ContactsContract.Contacts.CONTENT_MULTI_VCARD_URI,
                    Uri.encode(android.text.TextUtils.join(":", keys)));
            ByteArrayOutputStream bos = new ByteArrayOutputStream();
            try (InputStream in = getContentResolver().openInputStream(uri)) {
                if (in == null) return null;
                byte[] buf = new byte[8192];
                int n;
                while ((n = in.read(buf)) > 0) bos.write(buf, 0, n);
            }
            return bos.toByteArray();
        } catch (Throwable t) {
            Log.w("NowhereChooser", "buildContacts", t);
            out[0] = 0;
            return null;
        }
    }

    /** Build the calendar as a standard iCalendar (VCALENDAR/VEVENT). Returns the iCal bytes (null when none/error);
     *  event count -> out[0]. */
    private byte[] buildCalendarICal(int[] out) {
        try {
            StringBuilder ics = new StringBuilder();
            ics.append("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//nowhere//export//EN\r\nCALSCALE:GREGORIAN\r\n");
            int n = 0;
            String[] cols = { CalendarContract.Events._ID, CalendarContract.Events.TITLE,
                    CalendarContract.Events.DTSTART, CalendarContract.Events.DTEND,
                    CalendarContract.Events.EVENT_LOCATION, CalendarContract.Events.DESCRIPTION,
                    CalendarContract.Events.ALL_DAY, CalendarContract.Events.RRULE };
            try (Cursor c = getContentResolver().query(CalendarContract.Events.CONTENT_URI, cols,
                    null, null, CalendarContract.Events.DTSTART + " ASC")) {
                if (c == null) { out[0] = 0; return null; }
                while (c.moveToNext()) {
                    long id = c.getLong(0);
                    String title = c.getString(1);
                    if (c.isNull(2)) continue; // no start -> skip
                    long dtstart = c.getLong(2);
                    boolean hasEnd = !c.isNull(3);
                    long dtend = hasEnd ? c.getLong(3) : 0;
                    String loc = c.getString(4), desc = c.getString(5);
                    boolean allDay = c.getInt(6) == 1;
                    String rrule = c.getString(7);
                    ics.append("BEGIN:VEVENT\r\nUID:").append(id).append("@nowhere\r\n");
                    ics.append("SUMMARY:").append(icalEscape(title)).append("\r\n");
                    if (allDay) {
                        ics.append("DTSTART;VALUE=DATE:").append(icalDate(dtstart)).append("\r\n");
                        if (hasEnd) ics.append("DTEND;VALUE=DATE:").append(icalDate(dtend)).append("\r\n");
                    } else {
                        ics.append("DTSTART:").append(icalUtc(dtstart)).append("\r\n");
                        if (hasEnd) ics.append("DTEND:").append(icalUtc(dtend)).append("\r\n");
                    }
                    if (loc != null && !loc.isEmpty()) ics.append("LOCATION:").append(icalEscape(loc)).append("\r\n");
                    if (desc != null && !desc.isEmpty()) ics.append("DESCRIPTION:").append(icalEscape(desc)).append("\r\n");
                    if (rrule != null && !rrule.isEmpty()) ics.append("RRULE:").append(rrule).append("\r\n");
                    ics.append("END:VEVENT\r\n");
                    n++;
                }
            }
            ics.append("END:VCALENDAR\r\n");
            out[0] = n;
            if (n == 0) return null;
            return ics.toString().getBytes("UTF-8");
        } catch (Throwable t) {
            Log.w("NowhereChooser", "buildCalendar", t);
            out[0] = 0;
            return null;
        }
    }

    /** Build the text messages (SMS) as portable XML -- the de-facto "SMS Backup & Restore" shape: a <smses> root
     *  with one <sms> per message (address/date/type/read/body). MMS (multipart) is not included. Returns the bytes
     *  (null when none/error); count -> out[0]. Reads content://sms under READ_SMS; a denied grant just omits this. */
    private byte[] buildSmsXml(int[] out) {
        try {
            StringBuilder body = new StringBuilder();
            int n = 0;
            String[] cols = { "address", "date", "type", "read", "body" };
            try (Cursor c = getContentResolver().query(Uri.parse("content://sms"), cols, null, null, "date ASC")) {
                if (c == null) { out[0] = 0; return null; }
                int iA = c.getColumnIndex("address"), iD = c.getColumnIndex("date"),
                        iT = c.getColumnIndex("type"), iR = c.getColumnIndex("read"), iB = c.getColumnIndex("body");
                while (c.moveToNext()) {
                    body.append("  <sms address=\"").append(xmlAttr(iA >= 0 ? c.getString(iA) : ""))
                            .append("\" date=\"").append(iD >= 0 ? c.getLong(iD) : 0L)
                            .append("\" type=\"").append(iT >= 0 ? c.getInt(iT) : 0)
                            .append("\" read=\"").append(iR >= 0 ? c.getInt(iR) : 1)
                            .append("\" body=\"").append(xmlAttr(iB >= 0 ? c.getString(iB) : "")).append("\" />\n");
                    n++;
                }
            }
            out[0] = n;
            if (n == 0) return null;
            return ("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<smses count=\"" + n + "\">\n" + body + "</smses>\n")
                    .getBytes("UTF-8");
        } catch (Throwable t) {
            Log.w("NowhereChooser", "buildSms", t);
            out[0] = 0;
            return null;
        }
    }

    /** Build the call log as portable XML (<calls> root, one <call> per entry: number/date/duration/type). Returns
     *  the bytes (null when none/error); count -> out[0]. Reads CallLog under READ_CALL_LOG. */
    private byte[] buildCallLogXml(int[] out) {
        try {
            StringBuilder body = new StringBuilder();
            int n = 0;
            String[] cols = { android.provider.CallLog.Calls.NUMBER, android.provider.CallLog.Calls.DATE,
                    android.provider.CallLog.Calls.DURATION, android.provider.CallLog.Calls.TYPE };
            try (Cursor c = getContentResolver().query(android.provider.CallLog.Calls.CONTENT_URI, cols,
                    null, null, android.provider.CallLog.Calls.DATE + " ASC")) {
                if (c == null) { out[0] = 0; return null; }
                while (c.moveToNext()) {
                    body.append("  <call number=\"").append(xmlAttr(c.getString(0)))
                            .append("\" date=\"").append(c.getLong(1))
                            .append("\" duration=\"").append(c.getLong(2))
                            .append("\" type=\"").append(c.getInt(3)).append("\" />\n");
                    n++;
                }
            }
            out[0] = n;
            if (n == 0) return null;
            return ("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<calls count=\"" + n + "\">\n" + body + "</calls>\n")
                    .getBytes("UTF-8");
        } catch (Throwable t) {
            Log.w("NowhereChooser", "buildCallLog", t);
            out[0] = 0;
            return null;
        }
    }

    /** Escape a value for use inside an XML attribute (newlines preserved as &#10;). */
    private static String xmlAttr(String s) {
        if (s == null) return "";
        return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;").replace("\"", "&quot;")
                .replace("\r\n", "&#10;").replace("\n", "&#10;").replace("\r", "&#10;");
    }

    /** Zip the given files (name -> bytes) + a manifest into one portable bundle (what the web download hands back). */
    private byte[] buildBundleZip(java.util.LinkedHashMap<String, byte[]> entries, String manifestJson)
            throws java.io.IOException {
        ByteArrayOutputStream bos = new ByteArrayOutputStream();
        try (java.util.zip.ZipOutputStream zos = new java.util.zip.ZipOutputStream(bos)) {
            for (java.util.Map.Entry<String, byte[]> e : entries.entrySet()) {
                zos.putNextEntry(new java.util.zip.ZipEntry(e.getKey()));
                zos.write(e.getValue());
                zos.closeEntry();
            }
            zos.putNextEntry(new java.util.zip.ZipEntry("manifest.json"));
            zos.write(manifestJson.getBytes("UTF-8"));
            zos.closeEntry();
        }
        return bos.toByteArray();
    }

    /** Send the bundle to the daemon's length-prefixed EXPORT ("EXPORT\n<len>\n<bytes>") -> it seals + stores it. */
    private String sendExport(byte[] bundle) {
        try {
            LocalSocket s = new LocalSocket();
            s.connect(new LocalSocketAddress(SOCKET, LocalSocketAddress.Namespace.RESERVED));
            s.setSoTimeout(120000);
            OutputStream os = s.getOutputStream();
            os.write(("EXPORT\n" + bundle.length + "\n").getBytes("UTF-8"));
            os.write(bundle);
            os.flush();
            String line = new BufferedReader(new InputStreamReader(s.getInputStream(), "UTF-8")).readLine();
            s.close();
            return line == null ? "" : line.trim();
        } catch (Exception e) {
            Log.w("NowhereChooser", "sendExport", e);
            return "";
        }
    }

    /** Multi-line daemon request: returns the whole reply (every line) -- for verbs like GET-SNAPSHOTS that
     *  stream a list. (sendDaemon reads only the first line.) */
    private String sendDaemonMulti(String msg) {
        try {
            LocalSocket s = new LocalSocket();
            s.connect(new LocalSocketAddress(SOCKET, LocalSocketAddress.Namespace.RESERVED));
            s.setSoTimeout(90000);
            OutputStream os = s.getOutputStream();
            os.write(msg.getBytes("UTF-8"));
            os.flush();
            BufferedReader br = new BufferedReader(new InputStreamReader(s.getInputStream(), "UTF-8"));
            StringBuilder sb = new StringBuilder();
            String line;
            while ((line = br.readLine()) != null) sb.append(line).append('\n');
            s.close();
            return sb.toString();
        } catch (Exception e) {
            Log.w("NowhereChooser", "sendDaemonMulti", e);
            return "";
        }
    }

    /** "Restore a snapshot" (#58): list the profile's retained earlier heads; picking one rolls the store head
     *  back to it and signs out WITHOUT saving, so the next login lands on that earlier good version. */
    private void showSnapshots() {
        LinearLayout ll = menuScreen("Restore a snapshot", "go back to an earlier saved version");

        final TextView status = new TextView(this);
        status.setText("Loading…");
        status.setTextColor(MUTED);
        status.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        status.setGravity(Gravity.CENTER);
        status.setLineSpacing(dp(4), 1f);
        LinearLayout.LayoutParams stlp = new LinearLayout.LayoutParams(MATCH, WRAP);
        stlp.topMargin = dp(20);
        stlp.bottomMargin = dp(16);
        ll.addView(status, stlp);

        final LinearLayout rows = new LinearLayout(this);
        rows.setOrientation(LinearLayout.VERTICAL);
        ll.addView(rows, new LinearLayout.LayoutParams(MATCH, WRAP));

        TextView back = new TextView(this); // Back -> Your data (NOT the profile menu)
        back.setText("Back");
        back.setTextColor(ACCENT);
        back.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
        back.setGravity(Gravity.CENTER);
        back.setPadding(dp(8), dp(22), dp(8), dp(8));
        ll.addView(back, new LinearLayout.LayoutParams(MATCH, WRAP));
        back.setOnClickListener(v -> showYourData());

        android.widget.ScrollView sc = new android.widget.ScrollView(this);
        sc.setBackgroundColor(BG);
        sc.addView(ll);
        setContentView(sc);

        new Thread(() -> {
            final String resp = sendDaemonMulti("GET-SNAPSHOTS\n");
            runOnUiThread(() -> {
                String head = resp.trim();
                if (head.startsWith("NONE")) { status.setText("No active session."); return; }
                if (head.startsWith("LOCAL")) {
                    status.setText("This profile is a throwaway — nothing is saved to your store to restore.");
                    return;
                }
                if (head.isEmpty() || head.startsWith("EMPTY")) {
                    status.setText("No earlier versions yet. Snapshots build up as you use the phone.");
                    return;
                }
                status.setText("Pick a version to restore. Your current data on this device will be replaced, "
                        + "and you'll sign in again.");
                for (String line : resp.split("\n")) {
                    if (!line.startsWith("SNAP ")) continue;
                    String[] p = line.split(" ");
                    if (p.length < 3) continue;
                    final long ver, t;
                    try { ver = Long.parseLong(p[1]); t = Long.parseLong(p[2]); }
                    catch (NumberFormatException nfe) { continue; }
                    boolean manual = p.length >= 4 && "manual".equals(p[3]); // #58: pinned manual save vs auto-save
                    final String when = snapDate(t);
                    Button b = secondaryButton((manual ? "Backup · " : "Auto-save · ") + when, () -> confirmRollback(ver, when));
                    if (!manual) b.setTextColor(MUTED); // auto-saves are muted; manual saves keep the accent
                    rows.addView(b, btnLp(dp(8)));
                }
            });
        }).start();
    }

    private static String snapDate(long unixSecs) {
        if (unixSecs <= 0) return "earlier";
        return new java.text.SimpleDateFormat("MMM d, h:mm a", java.util.Locale.getDefault())
                .format(new java.util.Date(unixSecs * 1000L));
    }

    private void confirmRollback(final long version, final String label) {
        new android.app.AlertDialog.Builder(this)
                .setTitle("Restore this version?")
                .setMessage("This rolls your data back to the version saved " + label + ". Your current data on "
                        + "this device will be discarded and you'll be signed out — sign in again to load the "
                        + "earlier version.")
                .setPositiveButton("Restore", (d, w) -> doRollback(version))
                .setNegativeButton("Cancel", null)
                .show();
    }

    /** Send ROLLBACK; on OK the daemon rolls the store head + queues the switch+reap, so the user-0 watcher
     *  pulls us to the gate (no seal -> the current data can't clobber the snapshot). */
    private void doRollback(final long version) {
        TextView tv = new TextView(this);
        tv.setText("Restoring an earlier version…");
        tv.setTextColor(ACCENT);
        tv.setTextSize(TypedValue.COMPLEX_UNIT_SP, 24);
        tv.setTypeface(android.graphics.Typeface.DEFAULT_BOLD);
        tv.setGravity(Gravity.CENTER);
        tv.setBackgroundColor(BG);
        setContentView(tv);
        getWindow().addFlags(android.view.WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
        new Thread(() -> {
            final String r = sendDaemon("ROLLBACK\n" + version + "\n").trim();
            runOnUiThread(() -> {
                if (r.startsWith("OK")) {
                    tv.setText("Restoring an earlier version…\nSigning you out"); // the reap switches us to the gate
                } else {
                    getWindow().clearFlags(android.view.WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
                    String msg = r.startsWith("LOCAL") ? "This profile is a throwaway — there's nothing to restore."
                            : r.startsWith("NOSNAP") ? "That version is no longer available — pick another." // stale list, not a connection issue
                            : "Couldn't restore — check your connection and try again.";
                    android.widget.Toast.makeText(this, msg, android.widget.Toast.LENGTH_LONG).show();
                    showSnapshots(); // refresh -- the list may have changed since it was shown
                }
            });
        }).start();
    }

    private static String dateStamp() {
        return new java.text.SimpleDateFormat("yyyyMMdd-HHmmss", java.util.Locale.US).format(new java.util.Date());
    }

    private static String icalUtc(long millis) {
        java.text.SimpleDateFormat f = new java.text.SimpleDateFormat("yyyyMMdd'T'HHmmss'Z'", java.util.Locale.US);
        f.setTimeZone(java.util.TimeZone.getTimeZone("UTC"));
        return f.format(new java.util.Date(millis));
    }

    private static String icalDate(long millis) {
        java.text.SimpleDateFormat f = new java.text.SimpleDateFormat("yyyyMMdd", java.util.Locale.US);
        f.setTimeZone(java.util.TimeZone.getTimeZone("UTC"));
        return f.format(new java.util.Date(millis));
    }

    /** RFC 5545 text escaping for SUMMARY/LOCATION/DESCRIPTION values. */
    private static String icalEscape(String s) {
        if (s == null) return "";
        return s.replace("\\", "\\\\").replace(";", "\\;").replace(",", "\\,")
                .replace("\r\n", "\\n").replace("\n", "\\n").replace("\r", "\\n");
    }

    private void doLogoff() {
        String pn = profileName();
        TextView tv = new TextView(this);
        tv.setText(pn == null ? "Logging off…" : "Logging off " + pn + "…");
        tv.setTextColor(ACCENT);
        tv.setTextSize(TypedValue.COMPLEX_UNIT_SP, 28);
        tv.setTypeface(android.graphics.Typeface.DEFAULT_BOLD);
        tv.setGravity(Gravity.CENTER);
        tv.setBackgroundColor(BG);
        setContentView(tv);
        getWindow().addFlags(android.view.WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON); // don't sleep mid-logoff
        loggingOff = true; // from here on, block Back + re-front on Home/recents until the reap tears this down
        // ...and drop this task from Recents so it can't be swipe-CLOSED there mid-seal. #143 made the seal
        // SYNCHRONOUS, so this screen now lives for the whole upload (not ~1.5 s) -- long enough to wander into
        // recents and kill it. With Recents-exclusion + onUserLeaveHint (re-front) + Back-swallow, the screen
        // can't be escaped until the reap switches to the gate. (DIA-20260625-04)
        try {
            android.app.ActivityManager am = (android.app.ActivityManager) getSystemService(ACTIVITY_SERVICE);
            for (android.app.ActivityManager.AppTask t : am.getAppTasks()) t.setExcludeFromRecents(true);
        } catch (Throwable ignore) {}
        // (No Lock Task: it needs device-owner affiliation, and affiliating the device breaks roam-in -- the
        // unaffiliated ephemeral users get SetupWizard'd. Instead: Back-swallow + onUserLeaveHint re-front
        // below, AND the daemon's two-phase reap switches user-0's gate to the FOREGROUND in ~250ms (the user-0
        // chooser is android:persistent so its reap watcher isn't frozen mid-session and polls fast -- shrunk
        // from ~1.5s in DIA-20260618-07) -- so even the gesture-nav recents swipe, which onUserLeaveHint can't
        // catch, gets yanked back to the gate almost immediately and the session is gone. DIA-20260616-49.)

        new Thread(() -> {
            captureAppList(); // snapshot the user's apps so they roam + reprovision on the next login
            capturePrefs();   // snapshot this user's timezone + locale so they roam too (DIA-20260616-58)
            capturePerms();   // snapshot granted runtime perms (location etc.) so they roam too (DIA-20260625-06)
            boolean acked = false;
            try {
                LocalSocket s = new LocalSocket();
                s.connect(new LocalSocketAddress(SOCKET, LocalSocketAddress.Namespace.RESERVED));
                s.setSoTimeout(90000); // LOGOUT acks AT ONCE now (switch-first): the gate reclaims the foreground
                OutputStream os = s.getOutputStream();   // and shows the "Saving…" progress; the seal runs behind it.
                os.write("LOGOUT\n".getBytes("UTF-8"));
                os.flush();
                String reply = new BufferedReader(new InputStreamReader(s.getInputStream(), "UTF-8")).readLine();
                s.close();
                acked = reply != null && reply.startsWith("OK");
            } catch (Exception ignore) {
                // daemon unreachable -> fall through to the reboot fallback below.
            }
            if (acked) {
                // Stay in the SESSION showing "Saving your data… N%" while the background seal uploads: poll the
                // daemon (SEAL-STATUS) until DONE, then "Signing off…" + wait for the reap to switch+tear this
                // down. The user is NOT dumped to the gate during the upload. (DIA-20260625-04)
                // A logoff seals THREE parts in sequence (vault + #de + #media), each reporting its OWN 0->100
                // -- so the raw % ticked backward ("100 -> 99 -> 100") when the tiny tail parts started. Make it
                // MONOTONIC: ratchet Preparing up, then latch into Saving and ratchet that up only, ignoring the
                // tail parts' brief re-scans, so the bar never moves backward. (DIA-20260701-09)
                final int[] prepPct = {0}, savePct = {0};
                final boolean[] saving = {false};
                boolean sealFailed = false;
                while (true) {
                    String st = sealStatus();
                    if (st == null || st.startsWith("DONE")) break;
                    if (st.startsWith("FAILED")) { sealFailed = true; break; } // #86: daemon kept us signed in -> un-stick below
                    // Only render a line that HAS a percent. The seal emits transient 0% frames -- the empty
                    // start-up status, and the writeProg(0,total) at each phase's first frame -- which used to
                    // flash a number-less "Saving your data…" between the real readouts. Skip those: hold the
                    // prior line (the initial "Logging off…", then the last "… N%") so the readout only ever
                    // moves forward through "Preparing your data… N%" then "Saving your data… N%". (DIA-20260701-03)
                    if (st.startsWith("SEALING") && sealPct(st) > 0) {
                        int rawPct = sealPct(st);
                        boolean isSave = "save".equals(sealPhase(st));
                        // "prepare" = scan+chunk+dedup, "save" = upload. Ratchet each up, never down; once Saving
                        // starts, stay Saving (a tail part's re-scan can't drag it back to Preparing). (DIA-20260701-09)
                        if (isSave) {
                            saving[0] = true;
                            if (rawPct > savePct[0]) savePct[0] = rawPct;
                        } else if (!saving[0] && rawPct > prepPct[0]) {
                            prepPct[0] = rawPct;
                        }
                        final boolean sv = saving[0];
                        final int pct = sv ? savePct[0] : prepPct[0];
                        runOnUiThread(() -> {
                            tv.setTextSize(TypedValue.COMPLEX_UNIT_SP, 26);
                            // Label the two phases distinctly so the (long) prepare pass reads as progress, not a
                            // stuck "Saving…". (DIA-20260701-01)
                            String verb = sv ? "Saving your data" : "Preparing your data";
                            String head = verb + "…  " + pct + "%";
                            String warn = "\n\nKeep this screen open until it's done.\nLeaving now can lose your latest changes.";
                            android.text.SpannableString ss = new android.text.SpannableString(head + warn);
                            int hs = head.length(), he = ss.length();
                            // warning: ~0.55x of the 26sp head (≈14sp) + MUTED grey -- the saving line keeps the tv's
                            // base ACCENT teal/large so the two read as primary vs secondary. (DIA-20260625-05c)
                            ss.setSpan(new android.text.style.RelativeSizeSpan(0.55f), hs, he, android.text.Spannable.SPAN_EXCLUSIVE_EXCLUSIVE);
                            ss.setSpan(new android.text.style.ForegroundColorSpan(MUTED), hs, he, android.text.Spannable.SPAN_EXCLUSIVE_EXCLUSIVE);
                            tv.setText(ss);
                        });
                    }
                    try { Thread.sleep(400); } catch (InterruptedException ie) { break; }
                }
                if (sealFailed) {
                    // #86: the daemon's logoff seal FAILED and it kept the session SIGNED IN (did NOT reap unsealed
                    // data). Un-stick this screen -- re-enable Back/recents, drop keep-awake -- and return to the
                    // Profile menu with an error, instead of hanging on "Saving…" with no reap ever coming.
                    runOnUiThread(() -> {
                        loggingOff = false;
                        getWindow().clearFlags(android.view.WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
                        try {
                            android.app.ActivityManager am2 = (android.app.ActivityManager) getSystemService(ACTIVITY_SERVICE);
                            for (android.app.ActivityManager.AppTask tk : am2.getAppTasks()) tk.setExcludeFromRecents(false);
                        } catch (Throwable ignore) {}
                        android.widget.Toast.makeText(this,
                                "Couldn't save your data — you're still signed in. Check your connection and try again.",
                                android.widget.Toast.LENGTH_LONG).show();
                        showConfirm();
                    });
                    return; // no reap is coming -> do NOT fall through to "Signing off…"/reap-wait
                }
                runOnUiThread(() -> tv.setText("Signing off…"));
            }
            // On a clean ack the daemon's su:s0 worker has sealed and is reaping this profile: it switches
            // back to the gate on user 0 and stops+wipes this ephemeral user, which tears THIS screen down.
            // No reboot -- we just wait. Only if the daemon was unreachable do we fall back to a reboot, the
            // one wipe this secondary-user UI can force on its own (the user is ephemeral, so a boot wipes it).
            if (!acked) {
                try {
                    ((android.os.PowerManager) getSystemService(POWER_SERVICE)).reboot(null);
                } catch (Throwable t) {
                    startActivity(new Intent(this, ChooserActivity.class).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK));
                    finish();
                }
            }
        }).start();
    }

    /** P3 cold-lock (DIA-20260625-13): keep the session ON the device, FBE-locked + resumable, instead of
     *  sealing + wiping. Tell the daemon to COLD-LOCK -> it switches to the gate + STOPS (not removes) this user
     *  (CE key evicted -> /data ciphertext at rest). The stop tears this UI down; the gate offers RESUME (P4). */
    private void doColdLock() {
        TextView tv = new TextView(this);
        tv.setText("Locking…");
        tv.setTextColor(ACCENT);
        tv.setTextSize(TypedValue.COMPLEX_UNIT_SP, 28);
        tv.setTypeface(android.graphics.Typeface.DEFAULT_BOLD);
        tv.setGravity(Gravity.CENTER);
        tv.setBackgroundColor(BG);
        setContentView(tv);
        getWindow().addFlags(android.view.WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
        loggingOff = true; // unescapable until the stop tears it down (same swallow-Back + re-front as logoff)
        try {
            android.app.ActivityManager am = (android.app.ActivityManager) getSystemService(ACTIVITY_SERVICE);
            for (android.app.ActivityManager.AppTask t : am.getAppTasks()) t.setExcludeFromRecents(true);
        } catch (Throwable ignore) {}
        new Thread(() -> sendDaemon("COLD-LOCK\n")).start();
    }

    /** #4 (DIA-20260626-04): promote the throwaway into a real, roaming profile. The daemon creates its store
     *  vault + recovery code, drops the local-only flag, and seals the live data in the background; the session
     *  keeps running. On success we show the recovery code once. */
    private void doPromote() {
        new Thread(() -> {
            final String r = sendDaemon("PROMOTE\n").trim();
            runOnUiThread(() -> {
                if (r.startsWith("OK RECOVERY ")) {
                    sessionType = "STORED";
                    showRecovery(r.substring("OK RECOVERY ".length()).trim());
                } else if (r.startsWith("NOSTORE")) {
                    android.widget.Toast.makeText(this, "Set up a store first (Settings → Store) to save this profile",
                            android.widget.Toast.LENGTH_LONG).show();
                } else if (r.startsWith("ERR-EXISTS")) {
                    android.widget.Toast.makeText(this, "A profile with that name already exists in your store",
                            android.widget.Toast.LENGTH_LONG).show();
                } else if (r.startsWith("ERR-ALREADY")) {
                    sessionType = "STORED"; showConfirm(); // already saved -> just refresh
                } else if (r.startsWith("NOCREDIT")) {
                    // Saving a throwaway to the store costs storage credit and this device has none yet.
                    // Fold straight into Add-credits: redeem a claim code, then complete the save. (DIA-20260629-06)
                    showAddCreditsForSave();
                } else {
                    android.widget.Toast.makeText(this, "Couldn't save — try again", android.widget.Toast.LENGTH_LONG).show();
                }
            });
        }).start();
    }

    /** Show the 12-word recovery code once after a promote -- the ONLY way back in if the passphrase is forgotten
     *  (it's never stored). "Done" returns to the (now STORED) Profile screen. */
    private void showRecovery(String words) {
        LinearLayout ll = new LinearLayout(this);
        ll.setOrientation(LinearLayout.VERTICAL);
        ll.setGravity(Gravity.CENTER_HORIZONTAL);
        ll.setBackgroundColor(BG);
        ll.setPadding(dp(32), dp(48), dp(32), dp(36));

        TextView t = new TextView(this);
        t.setText("Saved — write this down");
        t.setTextColor(INK);
        t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 24);
        t.setGravity(Gravity.CENTER);
        ll.addView(t);

        TextView sub = new TextView(this);
        sub.setText("This is your recovery code — the only way back into this profile if you forget your "
                + "passphrase. No one, including us, can recover it for you. Keep it somewhere safe.");
        sub.setTextColor(MUTED);
        sub.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        sub.setGravity(Gravity.CENTER);
        sub.setLineSpacing(dp(3), 1f);
        LinearLayout.LayoutParams slp = new LinearLayout.LayoutParams(MATCH, WRAP);
        slp.topMargin = dp(14);
        slp.bottomMargin = dp(22);
        ll.addView(sub, slp);

        TextView code = new TextView(this);
        code.setText(words);
        code.setTextColor(ACCENT);
        code.setTextSize(TypedValue.COMPLEX_UNIT_SP, 18);
        code.setGravity(Gravity.CENTER);
        code.setTypeface(android.graphics.Typeface.MONOSPACE);
        code.setLineSpacing(dp(6), 1f);
        GradientDrawable cbg = new GradientDrawable();
        cbg.setColor(FIELD_BG);
        cbg.setCornerRadius(dp(13));
        cbg.setStroke(dp(1), FIELD_LN);
        code.setBackground(cbg);
        code.setPadding(dp(18), dp(20), dp(18), dp(20));
        LinearLayout.LayoutParams clp = new LinearLayout.LayoutParams(MATCH, WRAP);
        clp.bottomMargin = dp(28);
        ll.addView(code, clp);

        Button done = new Button(this);
        done.setText("Done");
        done.setAllCaps(false);
        done.setTextSize(TypedValue.COMPLEX_UNIT_SP, 15);
        done.setTextColor(ON_ACCENT);
        GradientDrawable dbg = new GradientDrawable();
        dbg.setColor(ACCENT);
        dbg.setCornerRadius(dp(13));
        done.setBackground(dbg);
        done.setStateListAnimator(null);
        done.setPadding(0, dp(14), 0, dp(14));
        ll.addView(done, new LinearLayout.LayoutParams(MATCH, WRAP));
        done.setOnClickListener(v -> showConfirm());

        android.widget.ScrollView sc = new android.widget.ScrollView(this);
        sc.setBackgroundColor(BG);
        sc.addView(ll);
        setContentView(sc);
    }

    /** Ask the daemon for the background logoff seal's status: "SEALING <phase> <done> <total>" or "DONE". */
    private String sealStatus() {
        try {
            LocalSocket s = new LocalSocket();
            s.connect(new LocalSocketAddress(SOCKET, LocalSocketAddress.Namespace.RESERVED));
            s.setSoTimeout(8000);
            s.getOutputStream().write("SEAL-STATUS\n".getBytes("UTF-8"));
            s.getOutputStream().flush();
            String line = new BufferedReader(new InputStreamReader(s.getInputStream(), "UTF-8")).readLine();
            s.close();
            return line;
        } catch (Exception e) {
            return null;
        }
    }

    /** "SEALING <phase> <done> <total>" -> the phase token ("prepare"|"save"|…), or "" if absent. */
    private static String sealPhase(String line) {
        try {
            String[] p = line.trim().split("\\s+");
            return p.length >= 4 ? p[1] : ""; // [SEALING, phase, done, total]
        } catch (Throwable t) {
            return "";
        }
    }

    /** "SEALING <phase> <done> <total>" (bytes) -> a 0–100 percent (long math). */
    private static int sealPct(String line) {
        try {
            String[] p = line.trim().split("\\s+");
            long done = Long.parseLong(p[p.length - 2]), total = Long.parseLong(p[p.length - 1]);
            if (total <= 0) return 0;
            long pct = done * 100 / total;
            return pct < 0 ? 0 : (pct > 100 ? 100 : (int) pct);
        } catch (Throwable t) {
            return 0;
        }
    }

    /** Once logoff has started the screen is unescapable: Back is swallowed... */
    @Override
    public void onBackPressed() {
        if (loggingOff) return; // swallow Back during logoff (before that, Back == Cancel/normal)
        super.onBackPressed();
    }

    /** ...and a Home/recents attempt bounces the screen straight back to the front, so you can't drop into
     *  the session that's being sealed + wiped. The reap (switch to the gate on user 0) is what ends it. */
    @Override
    protected void onUserLeaveHint() {
        super.onUserLeaveHint();
        if (loggingOff) {
            startActivity(new Intent(this, ProfileActivity.class)
                    .addFlags(Intent.FLAG_ACTIVITY_REORDER_TO_FRONT | Intent.FLAG_ACTIVITY_SINGLE_TOP));
        }
    }

    private int dp(float v) {
        return Math.round(v * getResources().getDisplayMetrics().density);
    }

    /** The "Change passphrase" screen for the logged-in profile: current + new + confirm -> daemon ROTATE. */
    private void showChangePass(final String name) {
        LinearLayout ll = new LinearLayout(this);
        ll.setOrientation(LinearLayout.VERTICAL);
        ll.setGravity(Gravity.CENTER_HORIZONTAL);
        ll.setBackgroundColor(BG);
        ll.setPadding(dp(32), dp(48), dp(32), dp(36));

        TextView t = new TextView(this);
        t.setText("Change passphrase");
        t.setTextColor(INK);
        t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 24);
        t.setGravity(Gravity.CENTER);
        ll.addView(t);

        TextView sub = new TextView(this);
        sub.setText("for " + name);
        sub.setTextColor(MUTED);
        sub.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
        sub.setGravity(Gravity.CENTER);
        LinearLayout.LayoutParams slp = new LinearLayout.LayoutParams(WRAP, WRAP);
        slp.topMargin = dp(4);
        slp.bottomMargin = dp(24);
        ll.addView(sub, slp);

        final EditText oldP = passField("current passphrase");
        ll.addView(oldP, fieldLp(dp(11)));
        final EditText newP = passField("new passphrase");
        ll.addView(newP, fieldLp(dp(11)));
        final EditText confP = passField("confirm new passphrase");
        ll.addView(confP, fieldLp(dp(20)));

        final Button change = accentBtn("Change passphrase");
        ll.addView(change, new LinearLayout.LayoutParams(MATCH, WRAP));

        final TextView msg = new TextView(this);
        msg.setTextColor(MUTED);
        msg.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        msg.setGravity(Gravity.CENTER);

        TextView cancel = new TextView(this);
        cancel.setText("Cancel");
        cancel.setTextColor(ACCENT);
        cancel.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
        cancel.setGravity(Gravity.CENTER);
        cancel.setPadding(dp(8), dp(18), dp(8), dp(8));
        ll.addView(cancel, new LinearLayout.LayoutParams(MATCH, WRAP));
        cancel.setOnClickListener(v -> showSecurity());

        // status/error message goes BELOW Cancel so it doesn't push the button + Cancel apart
        LinearLayout.LayoutParams mlp = new LinearLayout.LayoutParams(MATCH, WRAP);
        mlp.topMargin = dp(12);
        ll.addView(msg, mlp);

        change.setOnClickListener(v -> {
            final String op = oldP.getText().toString();
            final String np = newP.getText().toString();
            final String cp = confP.getText().toString();
            if (op.isEmpty() || np.isEmpty()) { msg.setTextColor(MUTED); msg.setText("fill in all fields"); return; }
            if (!np.equals(cp)) { msg.setTextColor(MUTED); msg.setText("new passphrases don't match"); return; }
            change.setEnabled(false);
            msg.setTextColor(ACCENT);
            msg.setText("changing…");
            new Thread(() -> {
                String r = sendDaemon("ROTATE\n" + name + "\n" + op + "\n" + np + "\n");
                runOnUiThread(() -> {
                    if (r.startsWith("OK")) {
                        oldP.setEnabled(false);
                        newP.setEnabled(false);
                        confP.setEnabled(false);
                        msg.setTextColor(ACCENT);
                        msg.setText("passphrase changed — use it next time you sign in");
                        change.setText("Done");
                        change.setEnabled(true);
                        change.setOnClickListener(w -> showSecurity());
                    } else {
                        change.setEnabled(true);
                        msg.setTextColor(MUTED);
                        msg.setText("couldn't change — check your current passphrase");
                    }
                });
            }).start();
        });

        setContentView(ll);
    }

    // ---- Storage & subscription (DIA-20260628-05). The roaming-store credit + lease state for this profile,
    // from the daemon's GET-BILLING (LOCAL wallet only -- no passphrase, no network, so it can't hang). Credit
    // is GB-months of unspent capacity; "paid through" is when rent is next due. GB-USED (the footprint) is a
    // later increment. Accountless by design: the "subscription" is just the local wallet + lease, nothing
    // server-side.
    private void showStorage() {
        LinearLayout ll = new LinearLayout(this);
        ll.setOrientation(LinearLayout.VERTICAL);
        ll.setBackgroundColor(BG);
        ll.setPadding(dp(32), dp(48), dp(32), dp(36));

        TextView t = new TextView(this);
        t.setText("Storage & subscription");
        t.setTextColor(INK);
        t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 24);
        t.setGravity(Gravity.CENTER);
        ll.addView(t, new LinearLayout.LayoutParams(MATCH, WRAP));

        final TextView usage = new TextView(this); // GB used -- loads async (GET-USAGE is a store round-trip)
        usage.setText("Calculating usage…");
        usage.setTextColor(ACCENT);
        usage.setTextSize(TypedValue.COMPLEX_UNIT_SP, 22);
        usage.setGravity(Gravity.CENTER);
        LinearLayout.LayoutParams ulp = new LinearLayout.LayoutParams(MATCH, WRAP);
        ulp.topMargin = dp(24);
        ll.addView(usage, ulp);

        // Device storage (#82): the store usage above is the roaming quota; THIS is how full the phone is (the
        // profile's local copy lives in /data). Distinct number, local statfs (no network), amber when nearly
        // full -- the guardrail for a store quota bigger than the device (e.g. 250GB profile on a 64GB phone).
        final TextView device = new TextView(this);
        device.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        device.setGravity(Gravity.CENTER);
        try {
            android.os.StatFs sf = new android.os.StatFs(getFilesDir().getPath());
            long total = sf.getTotalBytes(), free = sf.getAvailableBytes();
            int pct = total > 0 ? (int) ((total - free) * 100 / total) : 0;
            device.setText("This device: " + humanBytes(free) + " free of " + humanBytes(total) + " · " + pct + "% used");
            device.setTextColor(pct >= 80 ? 0xFFE0A34F : MUTED); // amber when nearly full
        } catch (Throwable ignore) { // NOT `t` -- collides with the `TextView t` above (the #82 fix that never reached main)
            device.setText("");
        }
        LinearLayout.LayoutParams dvlp = new LinearLayout.LayoutParams(MATCH, WRAP);
        dvlp.topMargin = dp(8);
        ll.addView(device, dvlp);

        final TextView body = new TextView(this); // credit/lease -- fast, local GET-BILLING
        body.setText("Loading…");
        body.setTextColor(MUTED);
        body.setTextSize(TypedValue.COMPLEX_UNIT_SP, 15);
        body.setGravity(Gravity.CENTER);
        body.setLineSpacing(dp(5), 1f);
        LinearLayout.LayoutParams blp = new LinearLayout.LayoutParams(MATCH, WRAP);
        blp.topMargin = dp(28);
        blp.bottomMargin = dp(28);
        ll.addView(body, blp);

        // "Add credits": redeem a claim code from a purchase into this profile's wallet (DIA-20260629-04).
        ll.addView(accentButton("Add credits", () -> showAddCredits()), btnLp(0));

        TextView back = new TextView(this);
        back.setText("Back");
        back.setTextColor(ACCENT);
        back.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
        back.setGravity(Gravity.CENTER);
        back.setPadding(dp(8), dp(20), dp(8), dp(8));
        ll.addView(back, new LinearLayout.LayoutParams(MATCH, WRAP));
        back.setOnClickListener(v -> showConfirm());

        android.widget.ScrollView sc = new android.widget.ScrollView(this);
        sc.setBackgroundColor(BG);
        sc.addView(ll);
        setContentView(sc);

        new Thread(() -> {
            final String r = sendDaemon("GET-BILLING\n").trim();
            runOnUiThread(() -> body.setText(formatBilling(r)));
        }).start();
        new Thread(() -> {
            final String u = sendDaemon("GET-USAGE\n").trim();
            runOnUiThread(() -> usage.setText(formatUsage(u)));
        }).start();
    }

    // ---- Add credits (DIA-20260629-04; throwaway-save fold-in DIA-20260629-06): redeem a paid claim code
    // into this session's wallet. The user buys storage on the nowhere storefront, which shows a one-time
    // claim code; they paste it here and the daemon (CLAIM) drains it into zero-knowledge blind tokens
    // (1 token = 1 GB-month). Two entry points share the screen: a STORED top-up (from Storage), and the
    // throwaway "Save to your store" path, where getting credit is immediately followed by the promote so
    // it's one flow. Accountless + unlinkable: the device redeems the code, the gateway can't tie the
    // credit to the payment.
    private void showAddCredits() { // STORED top-up, from Storage & subscription
        addCreditsScreen("Add credits", "scan the QR code, or paste the code or subscription key, from your purchase", false);
    }

    private void showAddCreditsForSave() { // throwaway: get credit, then complete Save-to-store
        addCreditsScreen("Save to your store", "saving needs storage credit — scan the QR code or paste the code from your purchase", true);
    }

    // QR scan (DIA-20260630-16): Add-credits can scan a code instead of typing it. QrScanActivity returns the
    // decoded text; we drop it into the field that launched the scan.
    private static final int REQ_SCAN = 4243;
    private EditText scanTargetField;

    @Override
    protected void onActivityResult(int req, int res, Intent data) {
        super.onActivityResult(req, res, data);
        if (req == REQ_SCAN && res == RESULT_OK && data != null && scanTargetField != null) {
            String code = data.getStringExtra(QrScanActivity.EXTRA_CODE);
            if (code != null && !code.trim().isEmpty()) scanTargetField.setText(code.trim());
        }
    }

    private void addCreditsScreen(String title, String subtitle, final boolean thenSave) {
        LinearLayout ll = menuScreen(title, subtitle);

        final EditText codeF = new EditText(this);
        codeF.setHint(thenSave ? "claim code" : "claim code or subscription key");
        codeF.setTextColor(FIELD_TX);
        codeF.setHintTextColor(HINT);
        codeF.setTextSize(TypedValue.COMPLEX_UNIT_SP, 16);
        codeF.setSingleLine(true);
        GradientDrawable fbg = new GradientDrawable();
        fbg.setColor(FIELD_BG);
        fbg.setCornerRadius(dp(12));
        fbg.setStroke(dp(1), FIELD_LN);
        codeF.setBackground(fbg);
        codeF.setPadding(dp(16), dp(14), dp(16), dp(14));
        LinearLayout.LayoutParams flp = new LinearLayout.LayoutParams(MATCH, WRAP);
        flp.topMargin = dp(26);
        ll.addView(codeF, flp);

        final Button add = accentBtn(thenSave ? "Add credits & save" : "Add credits");
        LinearLayout.LayoutParams alp = new LinearLayout.LayoutParams(MATCH, WRAP);
        alp.topMargin = dp(14);
        ll.addView(add, alp);

        // Scan a QR instead of typing (DIA-20260630-16): launches the Camera2 + ZXing scanner, drops the
        // decoded code into the field. Camera permission is requested on demand by QrScanActivity.
        final TextView scan = new TextView(this);
        scan.setText("Scan a QR code instead");
        scan.setTextColor(ACCENT);
        scan.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
        scan.setGravity(Gravity.CENTER);
        scan.setPadding(0, dp(14), 0, dp(2));
        LinearLayout.LayoutParams slp = new LinearLayout.LayoutParams(MATCH, WRAP);
        slp.topMargin = dp(2);
        ll.addView(scan, slp);
        scan.setOnClickListener(v -> {
            scanTargetField = codeF;
            try { startActivityForResult(new Intent(this, QrScanActivity.class), REQ_SCAN); }
            catch (Throwable t) {
                android.widget.Toast.makeText(this, "Couldn't open the scanner.", android.widget.Toast.LENGTH_SHORT).show();
            }
        });

        final TextView msg = new TextView(this);
        msg.setTextColor(MUTED);
        msg.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        msg.setGravity(Gravity.CENTER);
        msg.setLineSpacing(dp(4), 1f);
        LinearLayout.LayoutParams mlp = new LinearLayout.LayoutParams(MATCH, WRAP);
        mlp.topMargin = dp(16);
        ll.addView(msg, mlp);

        add.setOnClickListener(v -> {
            final String code = codeF.getText().toString().trim();
            if (code.isEmpty()) { msg.setTextColor(MUTED); msg.setText("Paste your code first."); return; }
            // A one-time claim code is 24 chars; a subscription key is 32 (base64url-raw of 18 vs 24 bytes).
            // The Save-to-store flow needs immediate credit, so it only accepts a one-time claim code.
            final boolean sub = !thenSave && code.length() == 32;
            add.setEnabled(false);
            codeF.setEnabled(false);
            msg.setTextColor(ACCENT);
            msg.setText(sub ? "Setting up subscription…" : "Adding credits…");
            new Thread(() -> {
                final String r = sendDaemon((sub ? "SUBSCRIBE\n" : "CLAIM\n") + code + "\n"); // may take a few seconds
                runOnUiThread(() -> {
                    if (!r.startsWith("OK")) {
                        add.setEnabled(true);
                        codeF.setEnabled(true);
                        final String m = r.startsWith("ERR-NOCODE")
                                ? "That code isn't valid, or it's already been used."
                                : r.startsWith("NONE") ? "No active session to add credits to."
                                : r.startsWith("LOCAL") ? "Save this throwaway to your store first, then subscribe."
                                : (sub ? "Couldn't set up the subscription — check your connection and try again."
                                       : "Couldn't add credits — check your connection and try again.");
                        msg.setTextColor(MUTED);
                        msg.setText(m);
                        return;
                    }
                    int parsed = 0;
                    try { parsed = Integer.parseInt(r.substring(2).trim()); } catch (NumberFormatException ignore) {}
                    final int got = parsed;
                    if (sub) {
                        // subscription secret stored in the wallet; got = tokens added THIS epoch (0 = the
                        // first refill is still pending the storefront's invoice). BACKUP roams the subkey.
                        new Thread(() -> sendDaemon("BACKUP\n")).start();
                        msg.setTextColor(ACCENT);
                        msg.setText(got > 0
                                ? "Subscription active — added " + got + " GB-" + (got == 1 ? "month" : "months")
                                        + ", and it refills automatically each month."
                                : "Subscription set up — your first refill arrives shortly, then automatically each month.");
                        add.setText("Done");
                        add.setEnabled(true);
                        add.setOnClickListener(w -> showStorage());
                    } else if (thenSave) {
                        // credit is now in the throwaway's wallet -> complete the save (promote pays with it)
                        msg.setText("Saving to your store…");
                        new Thread(() -> {
                            final String pr = sendDaemon("PROMOTE\n").trim();
                            runOnUiThread(() -> {
                                if (pr.startsWith("OK RECOVERY ")) {
                                    sessionType = "STORED";
                                    showRecovery(pr.substring("OK RECOVERY ".length()).trim());
                                } else {
                                    add.setEnabled(true);
                                    add.setText("Save to your store");
                                    add.setOnClickListener(w2 -> doPromote());
                                    msg.setTextColor(MUTED);
                                    msg.setText("Added " + got + " GB-" + (got == 1 ? "month" : "months")
                                            + ", but saving didn't finish — tap to try again.");
                                }
                            });
                        }).start();
                    } else {
                        new Thread(() -> sendDaemon("BACKUP\n")).start(); // roam the new credit at once
                        msg.setTextColor(ACCENT);
                        msg.setText("Added " + got + " GB-" + (got == 1 ? "month" : "months")
                                + " of storage — it's ready to use.");
                        add.setText("Done");
                        add.setEnabled(true);
                        add.setOnClickListener(w -> showStorage()); // back to the refreshed Storage view
                    }
                });
            }).start();
        });

        finishMenuScreen(ll); // adds the "Back" link (returns to the Profile menu)
    }

    /** "OK bytes=N" -> "X.X GB stored"; NONE/LOCAL/parse-fail -> a quiet dash. */
    private CharSequence formatUsage(String u) {
        if (u == null || !u.startsWith("OK")) return "—";
        long bytes = -1;
        for (String tok : u.split("\\s+")) {
            if (tok.startsWith("bytes=")) {
                try { bytes = Long.parseLong(tok.substring(6)); } catch (NumberFormatException ignore) {}
            }
        }
        return bytes < 0 ? "—" : humanBytes(bytes) + " stored";
    }

    private static String humanBytes(long b) {
        if (b >= (1L << 30)) return String.format(java.util.Locale.US, "%.1f GB", b / (double) (1L << 30));
        if (b >= (1L << 20)) return String.format(java.util.Locale.US, "%.0f MB", b / (double) (1L << 20));
        if (b >= (1L << 10)) return String.format(java.util.Locale.US, "%.0f KB", b / (double) (1L << 10));
        return b + " B";
    }

    /** Render a GET-BILLING reply ("OK credit=N through=E epochsec=S" | "NONE") into the Storage body. */
    private CharSequence formatBilling(String r) {
        if (r == null || !r.startsWith("OK")) {
            // No PAID wallet/subscription -- but that is NOT "not roaming". A promoted profile roams on the
            // free tier (token-less leases within the free allowance), so it IS kept alive without any
            // subscription. Only a throwaway (LOCAL, not yet promoted) truly isn't stored yet.
            if ("STORED".equals(sessionType)) {
                return "On the free tier — this profile roams within the free allowance, no subscription "
                        + "needed. Add credits to store more or keep it longer.";
            }
            return "No paid subscription. Save this profile and it roams on the free tier (free within the "
                    + "included allowance); add credits for more.";
        }
        long credit = -1, through = -1, epochsec = 604800;
        for (String tok : r.split("\\s+")) {
            int eq = tok.indexOf('=');
            if (eq < 0) continue;
            try {
                long v = Long.parseLong(tok.substring(eq + 1));
                switch (tok.substring(0, eq)) {
                    case "credit":   credit = v;   break;
                    case "through":  through = v;  break;
                    case "epochsec": epochsec = v; break;
                }
            } catch (NumberFormatException ignore) {}
        }
        StringBuilder sb = new StringBuilder();
        sb.append(credit < 0 ? "—" : credit + (credit == 1 ? " GB-month" : " GB-months")).append(" of credit");
        if (through > 0) {
            java.util.Date due = new java.util.Date((through + 1) * epochsec * 1000L);
            sb.append("\n\nPaid through ")
              .append(java.text.DateFormat.getDateInstance(java.text.DateFormat.MEDIUM).format(due))
              .append("\nRenews weekly");
        } else {
            sb.append("\n\nNot leased yet");
        }
        sb.append("\n\nYour subscription lives on this phone, in your roaming wallet — not in an account.");
        return sb;
    }

    // ---- Timezone picker (DIA-20260616-60; docs/timezone-model.md). Timezone follows WHERE YOU ARE -- automatic
    // by default, with an optional per-profile OVERRIDE the user picks here. A pick is (a) written to the roamed
    // prefs so it follows the identity (applied by the user-0 device owner on every login) AND (b) applied LIVE via
    // the daemon -> su:s0 worker (a secondary-user app can't set the global zone; root can, immediately). Picking
    // "Automatic" clears the override -> auto-detection (NITZ on a SIM device / geo) with the baked default as floor.
    private java.util.List<String> zoneIds; // canonical Olson IDs, sorted (lazy)

    private java.util.List<String> zones() {
        if (zoneIds == null) {
            java.util.ArrayList<String> z = new java.util.ArrayList<>();
            for (String id : java.util.TimeZone.getAvailableIDs()) {
                if (id.indexOf('/') < 0) continue;                       // drop bare "PST"/"UTC"
                if (id.startsWith("Etc/") || id.startsWith("SystemV/")) continue;
                z.add(id);
            }
            java.util.Collections.sort(z);
            zoneIds = z;
        }
        return zoneIds;
    }

    /** "America/New York  (GMT-05:00)" -- current offset incl. DST. */
    private String zoneLabel(String id) {
        int off = java.util.TimeZone.getTimeZone(id).getOffset(System.currentTimeMillis()) / 60000; // minutes
        char sign = off < 0 ? '-' : '+';
        off = Math.abs(off);
        return id.replace('_', ' ') + "  (GMT" + sign + String.format("%02d:%02d", off / 60, off % 60) + ")";
    }

    private void showTimezonePicker() {
        LinearLayout ll = new LinearLayout(this);
        ll.setOrientation(LinearLayout.VERTICAL);
        ll.setBackgroundColor(BG);
        ll.setPadding(dp(24), dp(40), dp(24), dp(24));

        TextView t = new TextView(this);
        t.setText("Time zone");
        t.setTextColor(INK);
        t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 24);
        t.setGravity(Gravity.CENTER);
        ll.addView(t, new LinearLayout.LayoutParams(MATCH, WRAP));

        String cur = readPrefValue("tz");
        TextView curTv = new TextView(this);
        curTv.setText("Currently: " + (cur.isEmpty() ? "Automatic (your location)" : cur));
        curTv.setTextColor(MUTED);
        curTv.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        curTv.setGravity(Gravity.CENTER);
        LinearLayout.LayoutParams clp = new LinearLayout.LayoutParams(MATCH, WRAP);
        clp.topMargin = dp(4); clp.bottomMargin = dp(16);
        ll.addView(curTv, clp);

        final EditText search = textField("search a city or region — e.g. Tokyo, London");
        ll.addView(search, fieldLp(dp(14)));

        final LinearLayout list = new LinearLayout(this);
        list.setOrientation(LinearLayout.VERTICAL);
        ll.addView(list, new LinearLayout.LayoutParams(MATCH, WRAP));

        final Runnable rebuild = new Runnable() {
            @Override public void run() {
                list.removeAllViews();
                String q = search.getText().toString().trim().toLowerCase();
                if (q.isEmpty() || "automatic".startsWith(q)) {
                    list.addView(zoneRow("Automatic — use your location", ""));
                }
                int shown = 0;
                for (String id : zones()) {
                    if (shown >= 60) break;
                    if (q.isEmpty() || id.toLowerCase().contains(q)) {
                        list.addView(zoneRow(zoneLabel(id), id));
                        shown++;
                    }
                }
                if (q.isEmpty()) {
                    TextView more = new TextView(ProfileActivity.this);
                    more.setText("…type to find more");
                    more.setTextColor(MUTED);
                    more.setTextSize(TypedValue.COMPLEX_UNIT_SP, 12);
                    more.setPadding(dp(6), dp(10), dp(6), dp(2));
                    list.addView(more);
                }
            }
        };
        search.addTextChangedListener(new android.text.TextWatcher() {
            @Override public void afterTextChanged(android.text.Editable s) { rebuild.run(); }
            @Override public void beforeTextChanged(CharSequence s, int a, int b, int c) {}
            @Override public void onTextChanged(CharSequence s, int a, int b, int c) {}
        });
        rebuild.run();

        TextView back = new TextView(this);
        back.setText("Back");
        back.setTextColor(ACCENT);
        back.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
        back.setGravity(Gravity.CENTER);
        back.setPadding(dp(8), dp(20), dp(8), dp(8));
        ll.addView(back, new LinearLayout.LayoutParams(MATCH, WRAP));
        back.setOnClickListener(v -> showPreferences());

        android.widget.ScrollView sc = new android.widget.ScrollView(this);
        sc.setBackgroundColor(BG);
        sc.addView(ll);
        setContentView(sc);
    }

    /** A tappable zone row; olson="" means Automatic. */
    private TextView zoneRow(String label, final String olson) {
        TextView r = new TextView(this);
        r.setText(label);
        r.setTextColor(INK);
        r.setTextSize(TypedValue.COMPLEX_UNIT_SP, 15);
        r.setPadding(dp(6), dp(13), dp(6), dp(13));
        r.setOnClickListener(v -> setTzAndApply(olson));
        return r;
    }

    /** Persist the override to the roamed prefs (so it follows the identity) AND apply it live via the daemon. */
    private void setTzAndApply(final String olson) {
        writeTzOverride(olson);
        new Thread(() -> sendDaemon("SET-TZ\n" + olson + "\n")).start(); // live: daemon -> su:s0 worker
        android.widget.Toast.makeText(this,
                olson.isEmpty() ? "Time zone: Automatic" : "Time zone: " + olson,
                android.widget.Toast.LENGTH_SHORT).show();
        showPreferences();
    }

    /** Override ONE roamed pref and rewrite nowhere-prefs whole, preserving every other key (tz / locale /
     *  idle / wipe) -- so changing one setting can never silently drop another. (DIA-20260626-03) */
    private void setPref(String key, String value) {
        String tz = "tz".equals(key) ? value : readPrefValue("tz");
        String loc = "locale".equals(key) ? value : readPrefValue("locale");
        if (loc.isEmpty()) loc = java.util.Locale.getDefault().toLanguageTag();
        String idle = "idle".equals(key) ? value : readPrefValue("idle");
        String wipe = "wipe".equals(key) ? value : readPrefValue("wipe");
        StringBuilder sb = new StringBuilder();
        sb.append("tz=").append(tz).append("\nlocale=").append(loc).append("\n");
        if (!idle.isEmpty()) sb.append("idle=").append(idle).append("\n");
        if (!wipe.isEmpty()) sb.append("wipe=").append(wipe).append("\n");
        try (java.io.FileWriter fw = new java.io.FileWriter(getFilesDir() + "/nowhere-prefs")) {
            fw.write(sb.toString());
        } catch (Exception e) {
            android.util.Log.w("NowhereChooser", "setPref " + key, e);
        }
    }

    /** Write the tz OVERRIDE (empty = automatic) into nowhere-prefs, preserving the other prefs. */
    private void writeTzOverride(String olson) { setPref("tz", olson); }

    // ---- L5 (DIA-20260623-26): per-profile idle auto-logoff timeout. Roams in nowhere-prefs (idle=<min>,
    // 0 = Never) like tz/locale; the user-0 gate reads it on the NEXT sign-in and arms the idle watcher with
    // it (the at-rest backstop from lock-model.md L3/L4). A high-security profile can pick a short timeout.
    private void showIdlePicker() {
        final int[] mins = { 2, 15, 30, 60, 0 };
        final String[] labels = { "After 2 minutes", "After 15 minutes", "After 30 minutes", "After 1 hour", "Never" };
        int cur = 15; // matches IDLE_LOGOFF_MS: the default when no idle= pref is set (DIA-20260626-03)
        try { String v = readPrefValue("idle"); if (!v.isEmpty()) cur = Integer.parseInt(v); } catch (Throwable ignore) {}
        int sel = 1; // default highlight = 15 minutes
        for (int i = 0; i < mins.length; i++) if (mins[i] == cur) sel = i;
        new android.app.AlertDialog.Builder(this)
                .setTitle("Sign out after inactivity")
                .setSingleChoiceItems(labels, sel, null)
                .setPositiveButton("Save", (d, w) -> {
                    int idx = ((android.app.AlertDialog) d).getListView().getCheckedItemPosition();
                    if (idx < 0) idx = 0;
                    writeIdlePref(mins[idx]);
                    android.widget.Toast.makeText(this, "Sign out after inactivity: " + labels[idx].toLowerCase()
                            + " — applies next sign-in", android.widget.Toast.LENGTH_LONG).show();
                    showSecurity();
                })
                .setNegativeButton("Cancel", (d, w) -> showSecurity())
                .show();
    }

    /** Write idle=<min> into nowhere-prefs, preserving the other prefs. */
    private void writeIdlePref(int minutes) { setPref("idle", String.valueOf(minutes)); }

    // ---- #3 (DIA-20260626-03): per-profile hard-wipe backstop. Roams in nowhere-prefs (wipe=<hours>,
    // 0 = keep until power-off) like idle; the user-0 gate reads it on the NEXT sign-in and arms the wipe
    // alarm AT cold-lock. Escalates a cold-locked, un-resumed session to a full amnesiac wipe (lock-model.md).
    private void showWipePicker() {
        final int[] hours = { 1, 6, 12, 24, 0 };
        final String[] labels = { "After 1 hour", "After 6 hours", "After 12 hours", "After 1 day", "Only on power-off" };
        int cur = 12; // matches HARD_WIPE_MS: the default when no wipe= pref is set
        try { String v = readPrefValue("wipe"); if (!v.isEmpty()) cur = Integer.parseInt(v); } catch (Throwable ignore) {}
        int sel = 2; // default highlight = 12 hours
        for (int i = 0; i < hours.length; i++) if (hours[i] == cur) sel = i;
        new android.app.AlertDialog.Builder(this)
                .setTitle("Erase a locked session after")
                .setSingleChoiceItems(labels, sel, null)
                .setPositiveButton("Save", (d, w) -> {
                    int idx = ((android.app.AlertDialog) d).getListView().getCheckedItemPosition();
                    if (idx < 0) idx = 0;
                    writeWipePref(hours[idx]);
                    android.widget.Toast.makeText(this, "Auto-wipe " + labels[idx].toLowerCase()
                            + " — applies next sign-in", android.widget.Toast.LENGTH_LONG).show();
                    showSecurity();
                })
                .setNegativeButton("Cancel", (d, w) -> showSecurity())
                .show();
    }

    /** Write wipe=<hours> into nowhere-prefs, preserving the other prefs. */
    private void writeWipePref(int hours) { setPref("wipe", String.valueOf(hours)); }

    /** A plain (non-password) single-line text field, styled like passField. */
    private EditText textField(String hint) {
        EditText e = new EditText(this);
        e.setHint(hint);
        e.setSingleLine(true);
        e.setTextColor(FIELD_TX);
        e.setHintTextColor(HINT);
        e.setTextSize(TypedValue.COMPLEX_UNIT_SP, 15);
        e.setInputType(android.text.InputType.TYPE_CLASS_TEXT);
        GradientDrawable g = new GradientDrawable();
        g.setColor(FIELD_BG);
        g.setCornerRadius(dp(13));
        g.setStroke(dp(1), FIELD_LN);
        e.setBackground(g);
        e.setPadding(dp(15), dp(14), dp(15), dp(14));
        return e;
    }

    /** "Delete this profile": a destructive confirm. The typed passphrase re-authenticates, then DELETE drops
     *  the profile (and its recovery ref) from the store and wipes the local session WITHOUT sealing -- so a
     *  deleted profile is never re-uploaded. Permanent: the recovery code stops working too. On OK the user-0
     *  chooser reaps this user (switch to the gate + remove), tearing this screen down like a logoff. */
    private void showDeleteConfirm(final String name) {
        final int RED = 0xFFE05B5B;
        LinearLayout ll = new LinearLayout(this);
        ll.setOrientation(LinearLayout.VERTICAL);
        ll.setGravity(Gravity.CENTER_HORIZONTAL);
        ll.setBackgroundColor(BG);
        ll.setPadding(dp(32), dp(48), dp(32), dp(36));

        TextView t = new TextView(this);
        t.setText("Delete profile");
        t.setTextColor(INK);
        t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 24);
        t.setGravity(Gravity.CENTER);
        ll.addView(t);

        TextView sub = new TextView(this);
        sub.setText(name);
        sub.setTextColor(MUTED);
        sub.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
        sub.setGravity(Gravity.CENTER);
        LinearLayout.LayoutParams slp = new LinearLayout.LayoutParams(WRAP, WRAP);
        slp.topMargin = dp(4);
        ll.addView(sub, slp);

        TextView warn = new TextView(this);
        warn.setText("This permanently removes this profile from your store and this device. It can't be "
                + "undone — the recovery code stops working too. Enter your passphrase to confirm.");
        warn.setTextColor(MUTED);
        warn.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        warn.setGravity(Gravity.CENTER);
        warn.setLineSpacing(dp(3), 1f);
        LinearLayout.LayoutParams wlp = new LinearLayout.LayoutParams(MATCH, WRAP);
        wlp.topMargin = dp(18);
        wlp.bottomMargin = dp(22);
        ll.addView(warn, wlp);

        final EditText passF = passField("passphrase");
        ll.addView(passF, fieldLp(dp(20)));

        final Button del = accentBtn("Delete permanently");
        GradientDrawable rbg = new GradientDrawable(); // override the teal accent -> read destructive
        rbg.setColor(RED);
        rbg.setCornerRadius(dp(13));
        del.setBackground(rbg);
        del.setTextColor(0xFFFFFFFF);
        ll.addView(del, new LinearLayout.LayoutParams(MATCH, WRAP));

        final TextView msg = new TextView(this);
        msg.setTextColor(MUTED);
        msg.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        msg.setGravity(Gravity.CENTER);

        TextView cancel = new TextView(this);
        cancel.setText("Cancel");
        cancel.setTextColor(ACCENT);
        cancel.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
        cancel.setGravity(Gravity.CENTER);
        cancel.setPadding(dp(8), dp(18), dp(8), dp(8));
        ll.addView(cancel, new LinearLayout.LayoutParams(MATCH, WRAP));
        cancel.setOnClickListener(v -> showConfirm());

        LinearLayout.LayoutParams mlp = new LinearLayout.LayoutParams(MATCH, WRAP);
        mlp.topMargin = dp(12);
        ll.addView(msg, mlp);

        del.setOnClickListener(v -> {
            final String pw = passF.getText().toString();
            if (pw.isEmpty()) { msg.setTextColor(MUTED); msg.setText("enter your passphrase to confirm"); return; }
            del.setEnabled(false);
            msg.setTextColor(RED);
            msg.setText("deleting…");
            new Thread(() -> {
                String r = sendDaemon("DELETE\n" + name + "\n" + pw + "\n");
                runOnUiThread(() -> {
                    if (r.startsWith("OK")) {
                        // Store ref dropped + the reap queued (no seal). Show a terminal screen and block escape;
                        // the user-0 chooser switches to the gate and removes this user, ending this screen.
                        loggingOff = true;
                        TextView done = new TextView(this);
                        done.setText("Profile deleted");
                        done.setTextColor(RED);
                        done.setTextSize(TypedValue.COMPLEX_UNIT_SP, 24);
                        done.setTypeface(android.graphics.Typeface.DEFAULT_BOLD);
                        done.setGravity(Gravity.CENTER);
                        done.setBackgroundColor(BG);
                        setContentView(done);
                        getWindow().addFlags(android.view.WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
                    } else {
                        del.setEnabled(true);
                        msg.setTextColor(MUTED);
                        msg.setText("couldn't delete — check your passphrase");
                    }
                });
            }).start();
        });

        setContentView(ll);
    }

    private EditText passField(String hint) {
        EditText e = new EditText(this);
        e.setHint(hint);
        e.setSingleLine(true);
        e.setTextColor(FIELD_TX);
        e.setHintTextColor(HINT);
        e.setTextSize(TypedValue.COMPLEX_UNIT_SP, 15);
        e.setInputType(android.text.InputType.TYPE_CLASS_TEXT | android.text.InputType.TYPE_TEXT_VARIATION_PASSWORD);
        GradientDrawable g = new GradientDrawable();
        g.setColor(FIELD_BG);
        g.setCornerRadius(dp(13));
        g.setStroke(dp(1), FIELD_LN);
        e.setBackground(g);
        e.setPadding(dp(15), dp(14), dp(15), dp(14));
        return e;
    }

    private Button accentBtn(String text) {
        Button b = new Button(this);
        b.setText(text);
        b.setAllCaps(false);
        b.setTextSize(TypedValue.COMPLEX_UNIT_SP, 15);
        b.setTextColor(ON_ACCENT);
        GradientDrawable bg = new GradientDrawable();
        bg.setColor(ACCENT);
        bg.setCornerRadius(dp(13));
        b.setBackground(bg);
        b.setStateListAnimator(null);
        b.setSoundEffectsEnabled(false);
        b.setPadding(0, dp(14), 0, dp(14));
        return b;
    }

    private LinearLayout.LayoutParams fieldLp(int bottom) {
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(MATCH, WRAP);
        lp.bottomMargin = bottom;
        return lp;
    }

    /** Send one line-framed request to the root login daemon and return its reply line. */
    private String sendDaemon(String msg) {
        try {
            LocalSocket s = new LocalSocket();
            s.connect(new LocalSocketAddress(SOCKET, LocalSocketAddress.Namespace.RESERVED));
            s.setSoTimeout(90000);
            OutputStream os = s.getOutputStream();
            os.write(msg.getBytes("UTF-8"));
            os.flush();
            BufferedReader br = new BufferedReader(new InputStreamReader(s.getInputStream(), "UTF-8"));
            String line = br.readLine();
            s.close();
            return line == null ? "" : line.trim();
        } catch (Exception e) {
            return "";
        }
    }

    /** Snapshot the launchable apps installed for the current (roamed) user into the chooser's own files
     *  dir, so the list rides in the sealed /data/user/N and the next login can reinstall them. */
    private void captureAppList() {
        try {
            Intent launch = new Intent(Intent.ACTION_MAIN).addCategory(Intent.CATEGORY_LAUNCHER);
            java.util.LinkedHashSet<String> pkgs = new java.util.LinkedHashSet<>();
            for (android.content.pm.ResolveInfo ri : getPackageManager().queryIntentActivities(launch, 0)) {
                String p = ri.activityInfo.packageName;
                if (!getPackageName().equals(p)) pkgs.add(p); // skip ourselves
            }
            StringBuilder sb = new StringBuilder();
            for (String p : pkgs) sb.append(p).append('\n');
            try (java.io.FileWriter fw = new java.io.FileWriter(getFilesDir() + "/nowhere-apps.list")) {
                fw.write(sb.toString());
            }
            android.util.Log.i("NowhereChooser", "captured " + pkgs.size() + " apps for roaming");
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "capture app list", t);
        }
    }

    /** Snapshot this (roamed) user's effective timezone + locale into the chooser's own files dir, so they
     *  ride in the sealed /data/user/N and the next login re-applies them (DIA-20260616-58). Mirrors
     *  captureAppList: getFilesDir() here = /data/user/N/com.nowhere.chooser/files (we run as the roamed
     *  user at logoff). The framework values are authoritative (the gate may have set tz via the device
     *  owner / locale via LocalePicker on login). One small line-oriented file, surfaced by roamd as
     *  prefs.out and applied by the user-0 gate on the next login. */
    private void capturePrefs() {
        try {
            // `tz` is the user's OVERRIDE, NOT the live system zone -- timezone is auto-by-location unless the
            // profile pins one (DIA-20260616-60; docs/timezone-model.md). PRESERVE any existing override (set by
            // the timezone picker); capturing the live tz would pin the auto-detected zone and defeat auto on the
            // next device. Locale DOES roam (identity-pinned), so capture the live locale.
            String tzOverride = readPrefValue("tz");
            String idle = readPrefValue("idle"); // L5: preserve the per-profile idle-logoff timeout across the seal
            String loc = java.util.Locale.getDefault().toLanguageTag();
            try (java.io.FileWriter fw = new java.io.FileWriter(getFilesDir() + "/nowhere-prefs")) {
                fw.write("tz=" + tzOverride + "\nlocale=" + loc + (idle.isEmpty() ? "" : "\nidle=" + idle) + "\n");
            }
            android.util.Log.i("NowhereChooser",
                    "captured prefs tzOverride=[" + tzOverride + "] locale=" + loc + " for roaming");
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "capture prefs", t);
        }
    }

    /** Snapshot each roamed app's GRANTED runtime (dangerous) permissions into files/nowhere-perms, so they
     *  ride in the sealed /data/user/N and roamd re-grants them on the next login (after install-existing) --
     *  runtime grants live in misc_de, OUTSIDE the sealed trees, so without this every login re-prompts (OM
     *  re-asks for location). Read here via PackageManager from the ROAMED user, which returns LIVE state --
     *  the on-disk runtime-permissions.xml is written lazily, so a su:s0 read at logoff is stale (it missed a
     *  just-granted location). One "pkg perm" per line; only dangerous + granted (an explicit deny is a fresh
     *  user's default, so it's preserved by simply not re-granting). (DIA-20260625-06) */
    private void capturePerms() {
        try {
            android.content.pm.PackageManager pm = getPackageManager();
            Intent launch = new Intent(Intent.ACTION_MAIN).addCategory(Intent.CATEGORY_LAUNCHER);
            java.util.LinkedHashSet<String> pkgs = new java.util.LinkedHashSet<>();
            for (android.content.pm.ResolveInfo ri : pm.queryIntentActivities(launch, 0)) {
                String p = ri.activityInfo.packageName;
                if (!getPackageName().equals(p)) pkgs.add(p); // skip ourselves
            }
            StringBuilder sb = new StringBuilder();
            for (String pkg : pkgs) {
                try {
                    android.content.pm.PackageInfo pi =
                            pm.getPackageInfo(pkg, android.content.pm.PackageManager.GET_PERMISSIONS);
                    if (pi.requestedPermissions == null) continue;
                    for (int i = 0; i < pi.requestedPermissions.length; i++) {
                        if ((pi.requestedPermissionsFlags[i]
                                & android.content.pm.PackageInfo.REQUESTED_PERMISSION_GRANTED) == 0) continue;
                        String perm = pi.requestedPermissions[i];
                        try { // only roam DANGEROUS (runtime) grants -- normal/signature perms aren't pm-grantable
                            if (pm.getPermissionInfo(perm, 0).getProtection()
                                    == android.content.pm.PermissionInfo.PROTECTION_DANGEROUS)
                                sb.append(pkg).append(' ').append(perm).append('\n');
                        } catch (Throwable ignore) {}
                    }
                } catch (Throwable ignore) {}
            }
            try (java.io.FileWriter fw = new java.io.FileWriter(getFilesDir() + "/nowhere-perms")) {
                fw.write(sb.toString());
            }
            android.util.Log.i("NowhereChooser", "captured runtime perms for roaming (" + sb.length() + " bytes)");
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "capturePerms", t);
        }
    }

    /** Read one `key=value` line from the roamed prefs file (`files/nowhere-prefs`), or "" if absent. */
    private String readPrefValue(String key) {
        try (java.io.BufferedReader br =
                     new java.io.BufferedReader(new java.io.FileReader(getFilesDir() + "/nowhere-prefs"))) {
            String line, p = key + "=";
            while ((line = br.readLine()) != null) {
                if (line.startsWith(p)) return line.substring(p.length()).trim();
            }
        } catch (Exception ignore) {
        }
        return "";
    }
}
