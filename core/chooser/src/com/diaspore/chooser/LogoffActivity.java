package com.diaspore.chooser;

import android.app.Activity;
import android.content.Intent;
import android.graphics.drawable.GradientDrawable;
import android.net.LocalSocket;
import android.net.LocalSocketAddress;
import android.os.Bundle;
import android.os.UserManager;
import android.util.TypedValue;
import android.view.Gravity;
import android.widget.Button;
import android.widget.EditText;
import android.widget.LinearLayout;
import android.widget.TextView;
import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.io.OutputStream;

/**
 * "Log off" entry in the launcher drawer. First shows a confirmation that NAMES the signed-in profile --
 * the user's name IS the Android user name (set at login from the profile name), so getUserName() surfaces
 * "who am I" with no extra plumbing. That makes this screen double as the see-current-profile affordance,
 * and the action reads "Log off <name>" rather than a bare "Log off". On confirm it tells the root login
 * daemon (LOGOUT) to seal the live session to the store, then reap this profile IN PLACE -- switch back to
 * the gate on user 0 and stop+wipe this ephemeral user (its /data is deleted and the FBE key destroyed) --
 * all WITHOUT a reboot, so logout takes seconds. If the daemon is unreachable it falls back to a reboot
 * (which also wipes the ephemeral user). The seal can touch the network, so it runs off the UI thread.
 * Cancel just returns to the home.
 */
public class LogoffActivity extends Activity {
    private static final String SOCKET = "diaspore_login";

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

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        showConfirm();
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

        LinearLayout ll = new LinearLayout(this);
        ll.setOrientation(LinearLayout.VERTICAL);
        ll.setGravity(Gravity.CENTER);
        ll.setBackgroundColor(BG);
        int padX = dp(32);
        ll.setPadding(padX, dp(40), padX, dp(40));

        TextView who = new TextView(this);
        who.setText(name == null ? "Signed in" : "Signed in as");
        who.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
        who.setTextColor(MUTED);
        who.setGravity(Gravity.CENTER);
        ll.addView(who);

        if (name != null) {
            TextView nameTv = new TextView(this);
            nameTv.setText(name);
            nameTv.setTextSize(TypedValue.COMPLEX_UNIT_SP, 30);
            nameTv.setTextColor(INK);
            nameTv.setGravity(Gravity.CENTER);
            nameTv.setLetterSpacing(0.02f);
            LinearLayout.LayoutParams nlp = new LinearLayout.LayoutParams(WRAP, WRAP);
            nlp.topMargin = dp(4);
            ll.addView(nameTv, nlp);
        }

        TextView warn = new TextView(this);
        warn.setText("Logging off seals your data to your store, then wipes this profile from the phone and "
                + "returns to the gate. Nothing stays on this device.");
        warn.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        warn.setTextColor(MUTED);
        warn.setGravity(Gravity.CENTER);
        warn.setLineSpacing(dp(3), 1f);
        LinearLayout.LayoutParams wlp = new LinearLayout.LayoutParams(MATCH, WRAP);
        wlp.topMargin = dp(20);
        wlp.bottomMargin = dp(28);
        ll.addView(warn, wlp);

        Button logoff = new Button(this);
        logoff.setText(name == null ? "Log off" : "Log off " + name);
        logoff.setAllCaps(false);
        logoff.setTextSize(TypedValue.COMPLEX_UNIT_SP, 15);
        logoff.setTextColor(ON_ACCENT);
        GradientDrawable bbg = new GradientDrawable();
        bbg.setColor(ACCENT);
        bbg.setCornerRadius(dp(13));
        logoff.setBackground(bbg);
        logoff.setStateListAnimator(null);
        logoff.setSoundEffectsEnabled(false); // no audio_service dependency on the click (defensive)
        logoff.setPadding(0, dp(14), 0, dp(14));
        ll.addView(logoff, new LinearLayout.LayoutParams(MATCH, WRAP));

        if (name != null) {
            TextView changePass = new TextView(this);
            changePass.setText("Change passphrase");
            changePass.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
            changePass.setTextColor(ACCENT);
            changePass.setGravity(Gravity.CENTER);
            changePass.setPadding(dp(8), dp(16), dp(8), dp(4));
            ll.addView(changePass, new LinearLayout.LayoutParams(MATCH, WRAP));
            changePass.setOnClickListener(v -> showChangePass(name));

            TextView tzRow = new TextView(this);
            tzRow.setText("Time zone");
            tzRow.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
            tzRow.setTextColor(ACCENT);
            tzRow.setGravity(Gravity.CENTER);
            tzRow.setPadding(dp(8), dp(10), dp(8), dp(4));
            ll.addView(tzRow, new LinearLayout.LayoutParams(MATCH, WRAP));
            tzRow.setOnClickListener(v -> showTimezonePicker());

            TextView del = new TextView(this);
            del.setText("Delete this profile");
            del.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
            del.setTextColor(0xFFE05B5B); // red -- destructive
            del.setGravity(Gravity.CENTER);
            del.setPadding(dp(8), dp(10), dp(8), dp(4));
            ll.addView(del, new LinearLayout.LayoutParams(MATCH, WRAP));
            del.setOnClickListener(v -> showDeleteConfirm(name));
        }

        TextView cancel = new TextView(this);
        cancel.setText("Cancel");
        cancel.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
        cancel.setTextColor(ACCENT);
        cancel.setGravity(Gravity.CENTER);
        cancel.setPadding(dp(8), dp(18), dp(8), dp(8));
        LinearLayout.LayoutParams clp = new LinearLayout.LayoutParams(MATCH, WRAP);
        clp.topMargin = dp(6);
        ll.addView(cancel, clp);

        setContentView(ll);

        logoff.setOnClickListener(v -> doLogoff());
        cancel.setOnClickListener(v -> finish());
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
        // (No Lock Task: it needs device-owner affiliation, and affiliating the device breaks roam-in -- the
        // unaffiliated ephemeral users get SetupWizard'd. Instead: Back-swallow + onUserLeaveHint re-front
        // below, AND the daemon's two-phase reap switches user-0's gate to the FOREGROUND in ~250ms (the user-0
        // chooser is android:persistent so its reap watcher isn't frozen mid-session and polls fast -- shrunk
        // from ~1.5s in DIA-20260618-07) -- so even the gesture-nav recents swipe, which onUserLeaveHint can't
        // catch, gets yanked back to the gate almost immediately and the session is gone. DIA-20260616-49.)

        new Thread(() -> {
            captureAppList(); // snapshot the user's apps so they roam + reprovision on the next login
            capturePrefs();   // snapshot this user's timezone + locale so they roam too (DIA-20260616-58)
            boolean acked = false;
            try {
                LocalSocket s = new LocalSocket();
                s.connect(new LocalSocketAddress(SOCKET, LocalSocketAddress.Namespace.RESERVED));
                s.setSoTimeout(90000); // LOGOUT seals /data/user/N to the store (S3 I/O) before it acks
                OutputStream os = s.getOutputStream();
                os.write("LOGOUT\n".getBytes("UTF-8"));
                os.flush();
                String reply = new BufferedReader(new InputStreamReader(s.getInputStream(), "UTF-8")).readLine();
                s.close();
                acked = reply != null && reply.startsWith("OK");
            } catch (Exception ignore) {
                // daemon unreachable -> fall through to the reboot fallback below.
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
            startActivity(new Intent(this, LogoffActivity.class)
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
        cancel.setOnClickListener(v -> showConfirm());

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
                        change.setOnClickListener(w -> finish());
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
                    TextView more = new TextView(LogoffActivity.this);
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
        back.setOnClickListener(v -> showConfirm());

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
        showConfirm();
    }

    /** Write the tz OVERRIDE into files/diaspore-prefs, preserving the roamed locale. */
    private void writeTzOverride(String olson) {
        String loc = readPrefValue("locale");
        if (loc.isEmpty()) loc = java.util.Locale.getDefault().toLanguageTag();
        try (java.io.FileWriter fw = new java.io.FileWriter(getFilesDir() + "/diaspore-prefs")) {
            fw.write("tz=" + olson + "\nlocale=" + loc + "\n");
        } catch (Exception e) {
            android.util.Log.w("DiasporeChooser", "writeTzOverride", e);
        }
    }

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
            try (java.io.FileWriter fw = new java.io.FileWriter(getFilesDir() + "/diaspore-apps.list")) {
                fw.write(sb.toString());
            }
            android.util.Log.i("DiasporeChooser", "captured " + pkgs.size() + " apps for roaming");
        } catch (Throwable t) {
            android.util.Log.w("DiasporeChooser", "capture app list", t);
        }
    }

    /** Snapshot this (roamed) user's effective timezone + locale into the chooser's own files dir, so they
     *  ride in the sealed /data/user/N and the next login re-applies them (DIA-20260616-58). Mirrors
     *  captureAppList: getFilesDir() here = /data/user/N/com.diaspore.chooser/files (we run as the roamed
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
            String loc = java.util.Locale.getDefault().toLanguageTag();
            try (java.io.FileWriter fw = new java.io.FileWriter(getFilesDir() + "/diaspore-prefs")) {
                fw.write("tz=" + tzOverride + "\nlocale=" + loc + "\n");
            }
            android.util.Log.i("DiasporeChooser",
                    "captured prefs tzOverride=[" + tzOverride + "] locale=" + loc + " for roaming");
        } catch (Throwable t) {
            android.util.Log.w("DiasporeChooser", "capture prefs", t);
        }
    }

    /** Read one `key=value` line from the roamed prefs file (`files/diaspore-prefs`), or "" if absent. */
    private String readPrefValue(String key) {
        try (java.io.BufferedReader br =
                     new java.io.BufferedReader(new java.io.FileReader(getFilesDir() + "/diaspore-prefs"))) {
            String line, p = key + "=";
            while ((line = br.readLine()) != null) {
                if (line.startsWith(p)) return line.substring(p.length()).trim();
            }
        } catch (Exception ignore) {
        }
        return "";
    }
}
