package com.nowhere.chooser;

import android.app.Activity;
import android.content.Context;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.content.pm.ResolveInfo;
import android.graphics.Canvas;
import android.graphics.Paint;
import android.graphics.drawable.GradientDrawable;
import android.net.LocalSocket;
import android.net.LocalSocketAddress;
import android.os.Bundle;
import android.text.InputType;
import android.util.TypedValue;
import android.view.Gravity;
import android.view.KeyEvent;
import android.view.View;
import android.widget.Button;
import android.widget.EditText;
import android.widget.LinearLayout;
import android.widget.TextView;
import java.io.BufferedReader;
import java.io.FileWriter;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.util.List;

/**
 * Nowhere blind-login chooser (P3.1). Shared gate for all editions (Diaspore/Endospore).
 *
 * Collects a profile name + passphrase and hands them to the ROOT login daemon over the AF_UNIX
 * socket /dev/socket/nowhere_login (created by init; the daemon is `nowhere_agent login-daemon`).
 * The daemon restores that profile's working set into the roaming tmpfs (/data/nowhere/state) and
 * replies "OK <n>" or "BLANK". The passphrase is sent in-memory over the socket -- it never touches
 * disk or any argv. An app can't do the restore itself (root-owned tmpfs / baked creds / S3 net), so
 * this socket is the privilege boundary.
 *
 * BLIND LOGIN: wrong passphrase OR unknown profile renders the SAME blank "-", indistinguishable, so
 * a hidden/duress profile is indistinguishable from a typo. A correct (name, passphrase) unlocks it.
 *
 * The UI is built in code (no res/ resources) and styled to the Nowhere brand: dark canvas, the
 * "nowhere" wordmark + a dispersing-spore motif, an accent, two blind-login fields (no profile
 * list -- the absence is the security property), and the amnesiac promise as a footer.
 */
public class ChooserActivity extends Activity {
    private static final String SOCKET = "nowhere_login";

    private static final int MATCH = LinearLayout.LayoutParams.MATCH_PARENT;
    private static final int WRAP  = LinearLayout.LayoutParams.WRAP_CONTENT;

    private static final int BG        = 0xFF0B0F14;
    private static final int INK       = 0xFFEAF1EE;
    private static final int MUTED     = 0xFF69788A;
    private static final int TAGLINE   = 0xFFDCE4EC;  // bright cool-white: the "your phone, nowhere" line
    private static final int FIELD_BG  = 0xFF121922;
    private static final int FIELD_LN  = 0xFF232E39;
    private static final int HINT      = 0xFF8595A4;
    private static final int FIELD_TX  = 0xFFE6EDF3;
    private static final int ACCENT    = 0xFF4FD6AC;
    private static final int ON_ACCENT = 0xFF05281F;
    private static final int FOOTER    = 0xFF505D69;

    private EditText profileField, passField, confirmField;
    private TextView result;
    private volatile long lastSealTs = 0; // "last active" unix ts streamed by the daemon on a returning login (DIA-20260625-05)
    private TextView toggle; // the create-profile / sign-in mode switch (locked during restore)
    private TextView gear; // top-right Settings gear; accent-tinted when an OS update is available (DIA-20260618-04)
    private android.content.BroadcastReceiver screenOffRx; // screen on/off: OTA auto-install + keyboard re-nudge
    private android.os.Handler otaTimer;   // periodic check for the preferred-time auto-install (DIA-20260619-01)
    private boolean otaTriggered;          // fire OTA-APPLY at most once per gate process
    private final Runnable otaTimeCheck = new Runnable() {
        @Override public void run() {
            new Thread(() -> {
                if (!otaTriggered && "1".equals(daemonCmd("GET-OTA-AUTO"))) {
                    String t = daemonCmd("GET-OTA-TIME");
                    if (!t.isEmpty() && nowWithinWindow(t) && isCharging()
                            && daemonCmd("OTA-STATUS").startsWith("AVAIL")) {
                        otaTriggered = true;
                        daemonCmd("OTA-APPLY");
                    }
                }
            }).start();
            android.os.Handler h = otaTimer;
            if (h != null) h.postDelayed(this, 15 * 60 * 1000L); // re-check every 15 min
        }
    };
    private Button unlockBtn;
    private View gateView;   // the main gate layout, kept so the recover/recovery-code screens can swap back
    private android.widget.ProgressBar progressBar; // determinate restore bar, driven by streamed progress
    private static final int MIN_PASS_LEN = 4; // OS rejects shorter as a lockscreen credential -> can't lock/resume
    private boolean createMode = false; // false = sign in; true = create a new profile
    private volatile boolean sawRestoreProgress = false; // #70/#75: the restore emitted progress -> the creds were accepted, so a later failure is a connection issue, not a wrong cred
    // P4 (DIA-20260625-13): resumable cold-lock. When the daemon reports a cold-locked session (GET-COLDLOCK),
    // the gate fronts a "Welcome back <name>" prompt -- passphrase only, name known -- that RESUMES the user
    // (verify CE in place -> switch in, no re-download) instead of a fresh restore. resumeAlt drops back to the
    // normal fresh-login gate ("not you? sign in"). The cold-lock switches to user 0 while asleep, so on wake
    // this is what the user sees -- the session's own lock screen, never the bare gate.
    private volatile boolean resumeMode = false;   // the gate is showing the cold-lock resume prompt
    private volatile boolean resumeBusy = false;   // a RESUME verify is in flight (don't re-enter / clobber)
    private String resumeUid = null, resumeName = null;
    private TextView subTitle, footerView, resumeAlt; // gate views toggled between fresh-login and resume modes
    // L1 (DIA-20260623-13): one-shot screen-off receiver that enrolls the live session's keyguard credential
    // on the FIRST screen-off (so login lands on home, not a lockscreen). Replaced/cancelled on each login.
    private static android.content.BroadcastReceiver sCredArmer;
    // L4 (DIA-20260623-17) idle-timeout backstop: screen-off arms an alarm, unlock cancels it; on fire the
    // idle+locked session auto-logs-off (the at-rest bound from lock-model.md L3). Replaced/cancelled per
    // login + on the logoff reap. Default 15 min (a gate-Settings override is a later L5 step).
    private static android.content.BroadcastReceiver sIdleScreenOff;
    private static android.content.BroadcastReceiver sIdleUserPresent;
    private static android.app.PendingIntent sIdleAlarmPi;
    // L4 (DIA-20260623-31, #29): true once the idle alarm is armed for the current lock; the alarm counts from
    // the FIRST screen-off after the last unlock, so a glance / notification (screen on->off without an
    // unlock) does NOT reset it. Cleared on unlock (USER_PRESENT) and on teardown.
    private static volatile boolean sIdleArmed;
    private static final long IDLE_LOGOFF_MS = 15 * 60 * 1000L; // default 15 min cold-lock, no `idle=` pref (DIA-20260626-03; was 1 h)
    // L5 (DIA-20260623-26): the active session's idle-logoff timeout (ms), from the roamed `idle=` pref
    // (0 = Never). Set by applyRoamedPrefs at login; armIdleWatcher uses it.
    private static volatile long sIdleTimeoutMs = IDLE_LOGOFF_MS;
    // #3 hard-wipe backstop (DIA-20260626-03): once a session COLD-LOCKS, a second, much longer alarm escalates
    // a still-un-resumed cold-locked session to a full amnesiac WIPE -- the same removal a power-off boot-wipe
    // would do, just on a timer instead of waiting for the power button. Armed at cold-lock; cancelled when a
    // session goes live again (armIdleWatcher). From the roamed `wipe=<hours>` pref (0 = Never).
    private static android.app.PendingIntent sWipeAlarmPi;
    private static final long HARD_WIPE_MS = 12 * 60 * 60 * 1000L; // default 12 h
    private static volatile long sWipeTimeoutMs = HARD_WIPE_MS;
    private boolean wantLocked = false; // intent: should the gate be kiosk-locked? (drives the retries below)
    private final android.os.Handler lockHandler = new android.os.Handler(android.os.Looper.getMainLooper());
    private int lockAttempts = 0;       // bounded lockGate retry: device-owner can be set LATE at first boot
    private final Runnable lockRetry = new Runnable() {
        @Override public void run() { if (wantLocked) lockGate(true); }
    };

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);

        LinearLayout ll = new LinearLayout(this);
        ll.setOrientation(LinearLayout.VERTICAL);
        ll.setGravity(Gravity.CENTER_HORIZONTAL);
        ll.setBackgroundColor(BG);
        int padX = dp(28);
        // Top padding nudged 72->110 (DIA-20260618-..): removing Store settings + Wi-Fi from the footer
        // (moved to the gear) shortened the column, leaving the content sitting high with a large bottom gap;
        // a bit more top space rebalances it. (fillViewport still lets it scroll when the keyboard is up.)
        ll.setPadding(padX, dp(110), padX, dp(40));

        SporeView spore = new SporeView(this);
        LinearLayout.LayoutParams spLp = new LinearLayout.LayoutParams(dp(72), dp(38));
        spLp.bottomMargin = dp(14);
        ll.addView(spore, spLp);

        TextView title = new TextView(this);
        title.setText(brand());
        title.setTextSize(TypedValue.COMPLEX_UNIT_SP, 46);
        title.setTextColor(0xFFFFFFFF);
        title.setGravity(Gravity.CENTER);
        title.setLetterSpacing(0.04f);
        ll.addView(title);

        TextView sub = new TextView(this);
        sub.setText("your phone, nowhere");
        sub.setTextSize(TypedValue.COMPLEX_UNIT_SP, 16);
        sub.setTextColor(TAGLINE);
        sub.setGravity(Gravity.CENTER);
        LinearLayout.LayoutParams subLp = new LinearLayout.LayoutParams(WRAP, WRAP);
        subLp.topMargin = dp(8);
        subLp.bottomMargin = dp(40);
        ll.addView(sub, subLp);
        subTitle = sub; // retargeted to "Welcome back, <name>" in resume mode

        profileField = field("profile", false);
        ll.addView(profileField, fieldLp(dp(11)));

        passField = field("passphrase", true);
        ll.addView(passField, fieldLp(dp(20)));

        confirmField = field("confirm passphrase", true);
        confirmField.setVisibility(View.GONE); // only shown in create mode
        ll.addView(confirmField, fieldLp(dp(20)));

        Button btn = new Button(this);
        btn.setText("Unlock");
        btn.setAllCaps(false);
        btn.setTextSize(TypedValue.COMPLEX_UNIT_SP, 15);
        btn.setTextColor(ON_ACCENT);
        GradientDrawable bbg = new GradientDrawable();
        bbg.setColor(ACCENT);
        bbg.setCornerRadius(dp(13));
        btn.setBackground(bbg);
        btn.setStateListAnimator(null);
        btn.setSoundEffectsEnabled(false); // no audio_service dependency on the click (defensive)
        btn.setPadding(0, dp(14), 0, dp(14));
        ll.addView(btn, new LinearLayout.LayoutParams(MATCH, WRAP));
        unlockBtn = btn;

        // Restore progress bar: hidden until a login starts, then driven by the daemon's streamed
        // "PROGRESS <phase> <done> <total>" lines so the gate shows real progress instead of freezing.
        progressBar = new android.widget.ProgressBar(this, null, android.R.attr.progressBarStyleHorizontal);
        progressBar.setMax(100);
        progressBar.setVisibility(View.GONE);
        progressBar.setProgressTintList(android.content.res.ColorStateList.valueOf(ACCENT));
        LinearLayout.LayoutParams pbLp = new LinearLayout.LayoutParams(MATCH, WRAP);
        pbLp.topMargin = dp(16);
        ll.addView(progressBar, pbLp);

        result = new TextView(this);
        result.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        result.setTextColor(MUTED);
        result.setGravity(Gravity.CENTER);
        LinearLayout.LayoutParams resLp = new LinearLayout.LayoutParams(WRAP, WRAP);
        resLp.topMargin = dp(18);
        ll.addView(result, resLp);

        TextView footer = new TextView(this);
        footer.setText("no account · nothing kept on this device");
        footer.setTextSize(TypedValue.COMPLEX_UNIT_SP, 12);
        footer.setTextColor(FOOTER);
        footer.setGravity(Gravity.CENTER);
        LinearLayout.LayoutParams ftLp = new LinearLayout.LayoutParams(WRAP, WRAP);
        ftLp.topMargin = dp(24);
        ll.addView(footer, ftLp);
        footerView = footer; // hidden in resume mode (the "nothing kept" line is the fresh-login pitch)

        toggle = new TextView(this);
        toggle.setText("New here?  Create a profile");
        toggle.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        toggle.setTextColor(ACCENT);
        toggle.setGravity(Gravity.CENTER);
        toggle.setPadding(dp(8), dp(12), dp(8), dp(8));
        LinearLayout.LayoutParams tgLp = new LinearLayout.LayoutParams(WRAP, WRAP);
        tgLp.topMargin = dp(6);
        ll.addView(toggle, tgLp);
        toggle.setOnClickListener(v -> {
            createMode = !createMode;
            confirmField.setVisibility(createMode ? View.VISIBLE : View.GONE);
            btn.setText(createMode ? "Create profile" : "Unlock");
            toggle.setText(createMode ? "Have a profile?  Sign in" : "New here?  Create a profile");
            result.setText("");
            confirmField.setText("");
        });

        // P4: resume-mode escape hatch -- "not you? sign in as someone else" drops the "Welcome back" prompt
        // back to the normal fresh-login gate (a different profile, or a deliberate fresh login). Hidden until
        // a cold-locked session is detected. The cold-locked user lingers until a logoff/boot-wipe reaps it.
        resumeAlt = new TextView(this);
        resumeAlt.setText("Not you?  Sign in as someone else");
        resumeAlt.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        resumeAlt.setTextColor(MUTED);
        resumeAlt.setGravity(Gravity.CENTER);
        resumeAlt.setPadding(dp(8), dp(12), dp(8), dp(8));
        resumeAlt.setVisibility(View.GONE);
        ll.addView(resumeAlt, new LinearLayout.LayoutParams(WRAP, WRAP));
        resumeAlt.setOnClickListener(v -> exitResumeMode());

        // "Forgot passphrase?" -> the recover flow (name + 12-word code -> a new passphrase).
        TextView forgot = new TextView(this);
        forgot.setText("Forgot your passphrase?");
        forgot.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        forgot.setTextColor(MUTED);
        forgot.setGravity(Gravity.CENTER);
        forgot.setPadding(dp(8), dp(2), dp(8), dp(8));
        ll.addView(forgot, new LinearLayout.LayoutParams(WRAP, WRAP));
        forgot.setOnClickListener(v -> setContentView(buildRecoverScreen()));

        // Store settings + Wi-Fi + Software update moved to the top-right Settings gear (buildSettingsScreen,
        // DIA-20260618-04) to declutter the gate footer. Emergency call stays HERE -- it must be reachable at
        // the locked gate (a kiosk has to allow 911); never bury it in settings.

        // "Emergency call" -> the system emergency dialer, reachable even at the locked gate (a kiosk must
        // allow 911). com.android.phone is allow-listed for Lock Task above, so this launches without
        // dropping the kiosk; the emergency dialer itself only permits emergency numbers.
        TextView emerg = new TextView(this);
        emerg.setText("Emergency call");
        emerg.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        emerg.setTextColor(0xFFE05B5B); // red -- always available
        emerg.setGravity(Gravity.CENTER);
        emerg.setPadding(dp(8), dp(10), dp(8), dp(8));
        ll.addView(emerg, new LinearLayout.LayoutParams(WRAP, WRAP));
        emerg.setOnClickListener(v -> {
            try {
                startActivity(new Intent("com.android.phone.EmergencyDialer.DIAL")
                        .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK));
            } catch (Throwable t) {
                android.util.Log.w("NowhereChooser", "emergency dial", t);
            }
        });

        // The gate SCROLLS. A long status message (e.g. "No internet yet — tap Wi-Fi to connect…", which wraps
        // to two lines) otherwise grows the column past the screen and pushes the footer links off the bottom
        // with no way to reach them. fillViewport keeps the column filling the screen when content is short
        // (normal layout unchanged) and only scrolls when it overflows. (DIA-20260616-59.)
        android.widget.ScrollView gateScroll = new android.widget.ScrollView(this);
        gateScroll.setBackgroundColor(BG);
        gateScroll.setFillViewport(true);
        gateScroll.addView(ll);

        // Top-right Settings gear (DIA-20260618-04): device-config (Store settings / Wi-Fi / Software update)
        // lives behind it so the gate footer stays the login essentials + Emergency call. It's an in-app
        // screen swap (setContentView), so it stays inside Lock-Task — no new activity, no kiosk escape. The
        // gear shows an accent dot when an OS update is available (checkForOta), keeping updates discoverable
        // without cluttering the gate.
        android.widget.FrameLayout gateFrame = new android.widget.FrameLayout(this);
        gateFrame.setBackgroundColor(BG);
        gateFrame.addView(gateScroll);
        gear = new TextView(this);
        gear.setText("⚙"); // ⚙ gear glyph
        gear.setTextColor(MUTED);
        gear.setTextSize(TypedValue.COMPLEX_UNIT_SP, 22);
        gear.setPadding(dp(18), dp(14), dp(18), dp(14));
        gateFrame.addView(gear, new android.widget.FrameLayout.LayoutParams(WRAP, WRAP, Gravity.TOP | Gravity.END));
        gear.setOnClickListener(v -> setContentView(buildSettingsScreen()));
        gateView = gateFrame;
        setContentView(gateView);

        btn.setOnClickListener(v -> {
            if (resumeMode) { resume(passField.getText().toString()); return; } // P4: resume a cold-locked session
            String n = profileField.getText().toString();
            String p = passField.getText().toString();
            // Min passphrase length: the OS rejects a <4-char string as a lockscreen credential, so a too-short
            // passphrase silently yields a session that can't lock OR cold-lock-resume. Reject it up front (empty
            // still falls through to the branch's specific "enter a passphrase" message). (DIA-20260625-13)
            if (!p.isEmpty() && p.length() < MIN_PASS_LEN) {
                result.setTextColor(MUTED); result.setText("passphrase must be at least " + MIN_PASS_LEN + " characters"); return;
            }
            if (createMode) {
                // #28: specific, actionable validation instead of a bare "—".
                if (n.isEmpty()) { result.setTextColor(MUTED); result.setText("enter a profile name"); return; }
                if (p.isEmpty()) { result.setTextColor(MUTED); result.setText("enter a passphrase"); return; }
                if (!p.equals(confirmField.getText().toString())) {
                    result.setTextColor(MUTED); result.setText("passphrases don't match"); return;
                }
                create(n, p);
            } else {
                if (n.isEmpty() || p.isEmpty()) {
                    result.setTextColor(MUTED); result.setText("enter your name and passphrase"); return;
                }
                unlock(n, p);
            }
        });

        // Headless driving: if launched with extras, prefill and auto-submit (used by the proof).
        Intent it = getIntent();
        String ep = it.getStringExtra("profile");
        String epw = it.getStringExtra("pass");
        if (ep != null && epw != null) {
            profileField.setText(ep);
            passField.setText(epw);
            unlock(ep, epw);
        }
        profileField.requestFocus();
        startReapWatcher(); // user-0 only: poll the daemon to do a no-reboot logoff's in-place user teardown
        startSyncStatusWatcher(); // user-0 only: surface a "not backed up" notification when periodic seals fail
        // First-run hint: with no store configured the device can't create/unlock yet -> point to Store settings.
        new Thread(() -> {
            if (sendDaemonLines("GET-STORE\n").contains("configured=no")) showStoreHint();
        }).start();
        // Name the primary user "Nowhere" -- it ships unnamed, so the system shows the generic "Owner"
        // (e.g. on the emergency-info card). Device-owner + platform-signed, so setUserName is permitted.
        try {
            android.os.UserManager um = (android.os.UserManager) getSystemService(USER_SERVICE);
            if (android.os.UserHandle.myUserId() == 0 && !brand().equals(um.getUserName())) {
                um.setUserName(brand());
            }
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "setUserName", t);
        }
        // NEVER set device-owner affiliation IDs. Tried it (DIA-48) so the logoff screen could Lock-Task itself
        // in the ephemeral roamed user -- but a DO that HAS affiliation IDs forces its UNaffiliated secondary
        // users (the roamed users, which can only affiliate too late) through the SetupWizard, which steals the
        // foreground and BREAKS roam-in/login. So keep affiliation CLEARED -- this also self-heals a device the
        // bad build left with it set. The logoff screen blocks Back + re-fronts on Home/recents instead.
        try {
            android.app.admin.DevicePolicyManager dpm =
                    (android.app.admin.DevicePolicyManager) getSystemService(DEVICE_POLICY_SERVICE);
            android.content.ComponentName admin = new android.content.ComponentName(this, AdminReceiver.class);
            if (dpm != null && dpm.isDeviceOwnerApp(getPackageName())) {
                java.util.Set<String> ids = dpm.getAffiliationIds(admin);
                if (ids != null && !ids.isEmpty()) {
                    dpm.setAffiliationIds(admin, java.util.Collections.emptySet());
                }
                // Brand the keyguard (DIA-20260617-05). The keyguard is mostly disabled -- the blind-login gate
                // IS the lock -- but it still appears for roamed ephemeral sessions on a screen-off, showing the
                // managed-device disclosure + an owner-info line over the dark brand wallpaper. Set both to the
                // Nowhere identity so the rare keyguard is on-brand. Device-owner APIs (idempotent), set on the
                // user-0 gate at device-owner scope, so it applies to the keyguard device-wide. (A full spore
                // GRAPHIC on the keyguard would need a per-user runtime FLAG_LOCK wallpaper on each ephemeral
                // roamed user -- no build-time default lock wallpaper exists in AOSP -- so that's deferred.)
                try { dpm.setOrganizationName(admin, brand()); } catch (Throwable t) {
                    android.util.Log.w("NowhereChooser", "setOrganizationName", t);
                }
                try { dpm.setDeviceOwnerLockScreenInfo(admin, "your phone, nowhere"); }
                catch (Throwable t) { android.util.Log.w("NowhereChooser", "setDeviceOwnerLockScreenInfo", t); }
            }
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "clearAffiliationIds", t);
        }
    }

    @Override
    protected void onNewIntent(Intent intent) {
        // The gate is a long-lived singleTask instance (and the device owner can't be force-stopped), so a
        // headless re-drive (am start --es profile/pass, used by the test harness) arrives HERE, not in
        // onCreate. Honor the same prefill + auto-submit so tests can drive a login.
        super.onNewIntent(intent);
        setIntent(intent);
        String ep = intent.getStringExtra("profile");
        String epw = intent.getStringExtra("pass");
        if (ep != null && epw != null) {
            profileField.setText(ep);
            passField.setText(epw);
            unlock(ep, epw);
        }
    }

    @Override
    protected void onResume() {
        super.onResume();
        sLiveGate = this; // so the reap watcher can update this gate when a no-reboot logoff finishes
        nudgeGateKeyboard(); // DIA-20260624-11: re-bind the IME after a logoff transition (no-keyboard-at-gate)
        lockGate(true); // boot-launched gate -> kiosk-lock until we confirm there's no active session
        // L4 (DIA-20260623-23): the gate is the ONLY auth -- no lockscreen in front of it. Boot keeps the
        // keyguard disabled, but the forced screen-wake after an idle/failed auto-logoff re-shows user 0's
        // insecure swipe keyguard in front of the gate. Show over it + dismiss it (insecure -> no auth) so the
        // wake lands straight on the gate, matching boot. A roamed session's SECURE keyguard is unaffected:
        // the gate is not foreground then, and requestDismissKeyguard cannot bypass a secure lock.
        try {
            // setShowWhenLocked turns this into a show-OVER-keyguard window, and Android SUPPRESSES the soft IME
            // for such a window -- so asserting it unconditionally on every gate resume killed the profile/
            // passphrase keyboard at the interactive gate (DIA-20260624-11; "transient" because the IME attaches
            // or not depending on keyguard/focus timing). We only need show-over-lock to get PAST a keyguard:
            // the forced-wake-after-auto-logoff path re-shows user 0's INSECURE swipe keyguard in front of the
            // gate. So assert it only when a keyguard is actually up, dismiss it, then DROP the flag the instant
            // it's gone -- and on the normal interactive gate (boot keeps the keyguard disabled) never set it, so
            // the keyboard attaches. (setTurnScreenOn stays unconditional: harmless when already awake, needed on
            // the forced wake.) A roamed session's SECURE keyguard is unaffected -- the gate isn't foreground then.
            android.app.KeyguardManager km = (android.app.KeyguardManager) getSystemService(KEYGUARD_SERVICE);
            boolean locked = (km != null && km.isKeyguardLocked());
            if (android.os.Build.VERSION.SDK_INT >= 27) { setTurnScreenOn(true); setShowWhenLocked(locked); }
            if (locked) {
                km.requestDismissKeyguard(this, new android.app.KeyguardManager.KeyguardDismissCallback() {
                    @Override public void onDismissSucceeded() {
                        // keyguard gone -> stop being a show-when-locked window so the IME can attach to the fields
                        runOnUiThread(() -> { if (android.os.Build.VERSION.SDK_INT >= 27) setShowWhenLocked(false); });
                    }
                });
            }
        } catch (Throwable t) { android.util.Log.w("NowhereChooser", "gate keyguard dismiss", t); }
        applySigningOutUi(); // if a logoff's reap is still finishing, show "Signing out…" (the gate looks frozen)
        // Privacy: a gate AT REST must not be able to obtain a location fix. The Wi-Fi scan turns the
        // location MASTER toggle on; turn it back off whenever the gate is (re)foregrounded. We flip only the
        // toggle, never the runtime FINE_LOCATION grant -- revoking a held runtime permission kills this
        // persistent process and the relaunch doesn't land cleanly on the gate (see stopScanLocation).
        stopScanLocation();
        // launcher3 is the home now; the chooser is only launched (by BootReceiver) as the boot gate.
        // If a session is somehow already active (a spurious relaunch over a logged-in launcher), don't
        // re-gate -- drop the lock and finish, returning to that session.
        //
        // SECURITY (kiosk escape): finishing the gate drops Lock-Task, and on user 0 that reveals user 0's
        // launcher3 -- from which Settings -> enable adb (and worse) is reachable. That's only acceptable when
        // a SECONDARY user is genuinely the foreground user (finish returns to its session). So gate the finish
        // on the ACTUAL current user, not just the daemon's "a session exists" STATUS: a slow-restore race once
        // left a secondary user briefly "active" while user 0 was still foreground, the gate finished, and Home
        // stranded on user 0's launcher3. On user 0 the gate must STAY, kiosk-locked. (DIA-20260625-13)
        new Thread(() -> {
            boolean active = "ACTIVE".equals(queryStatus());
            int cur = currentForegroundUser();
            if (active && cur > 0) {
                runOnUiThread(() -> { lockGate(false); finish(); });
            } else if (active && cur == 0) {
                android.util.Log.w("NowhereChooser", "STATUS=ACTIVE but current user is 0 -> NOT finishing (would strand on launcher3)");
            }
        }).start();
        // P4: surface a cold-locked session as the "Welcome back" resume prompt rather than the bare gate. The
        // cold-lock switched to user 0 while the device slept, so this onResume is exactly what the user wakes
        // into. No cold-lock -> reset to the standard login form. Skipped while a logoff reap or a resume owns
        // the UI (signingOut / resumeBusy).
        if (!signingOut && !resumeBusy) {
            new Thread(() -> {
                String cl = sendDaemon("GET-COLDLOCK\n").trim();
                // #73: while parked, reap leftover roam users (failed/replaced logins, superseded cold-locks) so
                // no orphaned identity data lingers and at most one roam user is kept (the resumable cold-lock).
                // Act ONLY on a definitive answer: "NONE" -> keep nothing; "COLDLOCK <uid> …" -> keep that uid.
                // An empty/garbled reply (daemon hiccup) or an unparseable uid skips the sweep, so we never reap
                // a cold-lock we couldn't positively identify.
                if (cl.equals("NONE")) {
                    reapOrphanSecondaryUsers(-1);
                } else if (cl.startsWith("COLDLOCK ")) {
                    String rest = cl.substring(9).trim();
                    int sp = rest.indexOf(' ');
                    if (sp > 0) {
                        try { reapOrphanSecondaryUsers(Integer.parseInt(rest.substring(0, sp))); }
                        catch (NumberFormatException ignore) {} // unparseable uid -> skip (don't risk the cold-lock)
                    }
                }
                runOnUiThread(() -> {
                    if (resumeBusy || signingOut) return;
                    if (cl.startsWith("COLDLOCK ")) {
                        String rest = cl.substring(9).trim(); // "<uid> <name>" -- uid first so a spaced name survives
                        int sp = rest.indexOf(' ');
                        if (sp > 0) enterResumeMode(rest.substring(sp + 1), rest.substring(0, sp));
                    } else {
                        exitResumeMode();
                    }
                });
            }).start();
        }
        checkForOta(); // accent the Settings gear if a newer OS is published
        // Auto-install OTA when parked (DIA-20260618-06): if the user enabled it, install while charging AND
        // the screen is off (never mid-use). This is the "Any time" path -- when a preferred TIME is set
        // (DIA-20260619-01) the timer below handles it instead, so the screen-off path stands down.
        if (screenOffRx == null) {
            screenOffRx = new android.content.BroadcastReceiver() {
                @Override public void onReceive(android.content.Context c, Intent i) {
                    if (Intent.ACTION_SCREEN_ON.equals(i.getAction())) {
                        // DIA-20260624-11 (recurrence): the IME binding to the gate goes stale when a logoff reap
                        // brings the gate up with the SCREEN OFF -- the nudge can't render (no display) and onResume
                        // won't re-fire on wake, so the keyboard is missing. Re-nudge on EVERY screen-on while THIS
                        // is the live foreground gate, so the keyboard re-binds whenever the gate becomes visible
                        // (covers re-staleness after the reap's brief wakelock too). Cost is a tiny keyboard
                        // re-cycle on an idle-gate wake; nudgeGateKeyboard no-ops while signing out / a resume runs.
                        if (sLiveGate == ChooserActivity.this) nudgeGateKeyboard();
                        return;
                    }
                    // ACTION_SCREEN_OFF: auto-install a parked OTA (DIA-20260618-06).
                    new Thread(() -> {
                        if (!otaTriggered && "1".equals(daemonCmd("GET-OTA-AUTO"))
                                && daemonCmd("GET-OTA-TIME").isEmpty() // a set time uses the timer, not screen-off
                                && isCharging() && daemonCmd("OTA-STATUS").startsWith("AVAIL")) {
                            otaTriggered = true;
                            daemonCmd("OTA-APPLY");
                        }
                    }).start();
                }
            };
            android.content.IntentFilter scr = new android.content.IntentFilter(Intent.ACTION_SCREEN_OFF);
            scr.addAction(Intent.ACTION_SCREEN_ON);
            registerReceiver(screenOffRx, scr, android.content.Context.RECEIVER_NOT_EXPORTED);
        }
        // Preferred-time auto-install (DIA-20260619-01): the gate process is persistent (-800), so it isn't
        // frozen with the screen off -- a periodic check installs around the chosen time while charging.
        if (otaTimer == null) {
            otaTimer = new android.os.Handler(android.os.Looper.getMainLooper());
            otaTimer.postDelayed(otaTimeCheck, 60 * 1000L); // first check ~1 min after foreground, then every 15 min
        }
    }

    @Override
    protected void onPause() {
        super.onPause();
        if (sLiveGate == this) sLiveGate = null;
    }

    @Override
    protected void onDestroy() {
        super.onDestroy();
        if (screenOffRx != null) {
            try { unregisterReceiver(screenOffRx); } catch (Exception ignored) {}
            screenOffRx = null;
        }
        if (otaTimer != null) {
            otaTimer.removeCallbacks(otaTimeCheck);
            otaTimer = null;
        }
    }

    // DIA-20260624-11: after a logoff transition the IME intermittently fails to re-bind to the gate
    // (input_method mCurRootView=null) -> the gate shows but has no soft keyboard until a manual focus / lock-
    // unlock. (Verified NOT an instance pile-up -- there is only ever one gate -- and not show-when-locked when the
    // keyguard isn't up.) Nudge it once the window settles: drop any leftover show-when-locked (it suppresses the
    // soft IME), re-focus the visible input field, and re-request the keyboard so the IME re-binds. No-op when the
    // keyboard is already up; skipped while signing out / a resume is in flight (no input field then).
    private void nudgeGateKeyboard() {
        getWindow().getDecorView().postDelayed(() -> {
            try {
                if (signingOut || resumeBusy) return;
                android.app.KeyguardManager km2 = (android.app.KeyguardManager) getSystemService(KEYGUARD_SERVICE);
                if (android.os.Build.VERSION.SDK_INT >= 27 && (km2 == null || !km2.isKeyguardLocked())) setShowWhenLocked(false);
                final android.widget.EditText f = (profileField != null && profileField.getVisibility() == View.VISIBLE)
                        ? profileField : passField;
                if (f == null) return;
                final android.view.inputmethod.InputMethodManager imm =
                        (android.view.inputmethod.InputMethodManager) getSystemService(INPUT_METHOD_SERVICE);
                if (imm == null) return;
                // After a logoff the IME reports "shown" (mInputShown=true, mCurRootView set) but the keyboard
                // doesn't RENDER -- on FP3 only a full lock-unlock recovers it (Chester). Replicate that cycle:
                // restart the input connection, HIDE the stale IME window, then re-focus + SHOW so it's recreated.
                f.requestFocus();
                imm.restartInput(f);
                imm.hideSoftInputFromWindow(f.getWindowToken(), 0);
                f.postDelayed(() -> {
                    try {
                        f.requestFocus();
                        imm.showSoftInput(f, android.view.inputmethod.InputMethodManager.SHOW_IMPLICIT);
                    } catch (Throwable ignore) {}
                }, 250);
            } catch (Throwable t) { android.util.Log.w("NowhereChooser", "IME re-bind nudge", t); }
        }, 500);
    }

    /** True when the device is on external power (AC/USB/wireless). Read from the sticky battery-changed
     *  broadcast -- no permission needed -- so the auto-OTA watcher only installs while charging. */
    private boolean isCharging() {
        Intent bs = registerReceiver(null,
                new android.content.IntentFilter(Intent.ACTION_BATTERY_CHANGED));
        if (bs == null) return false;
        return bs.getIntExtra(android.os.BatteryManager.EXTRA_PLUGGED, 0) != 0;
    }

    /** True when the local clock is within the hour starting at the preferred "HH:MM" install time. With the
     *  15-min poll this fires within ~15 min of the chosen time. */
    private boolean nowWithinWindow(String hhmm) {
        try {
            int set = Integer.parseInt(hhmm.substring(0, 2)) * 60 + Integer.parseInt(hhmm.substring(3, 5));
            java.util.Calendar c = java.util.Calendar.getInstance();
            int now = c.get(java.util.Calendar.HOUR_OF_DAY) * 60 + c.get(java.util.Calendar.MINUTE);
            int diff = now - set;
            return diff >= 0 && diff < 60;
        } catch (Exception e) {
            return false;
        }
    }

    /** "HH:MM" (24h) -> a friendly 12h label like "3:00 AM". */
    private String prettyTime(String hhmm) {
        try {
            int h = Integer.parseInt(hhmm.substring(0, 2));
            int m = Integer.parseInt(hhmm.substring(3, 5));
            int h12 = h % 12; if (h12 == 0) h12 = 12;
            return String.format(java.util.Locale.US, "%d:%02d %s", h12, m, h < 12 ? "AM" : "PM");
        } catch (Exception e) {
            return hhmm;
        }
    }

    /** Dialog to pick the preferred install time; the neutral "Any time" button clears it (back to the
     *  default next-screen-off-while-charging behaviour). */
    private void showOtaTimePicker(Runnable onChanged) {
        new Thread(() -> {
            final String cur = daemonCmd("GET-OTA-TIME");
            final int ch = cur.length() == 5 ? Integer.parseInt(cur.substring(0, 2)) : 3;
            final int cm = cur.length() == 5 ? Integer.parseInt(cur.substring(3, 5)) : 0;
            runOnUiThread(() -> {
                final android.widget.TimePicker tp = new android.widget.TimePicker(this);
                tp.setIs24HourView(false);
                tp.setHour(ch);
                tp.setMinute(cm);
                new android.app.AlertDialog.Builder(this)
                        .setTitle("Install time (while charging)")
                        .setView(tp)
                        .setPositiveButton("Set", (d, w) -> {
                            final String hhmm = String.format(java.util.Locale.US, "%02d:%02d", tp.getHour(), tp.getMinute());
                            new Thread(() -> { daemonCmd("SET-OTA-TIME\n" + hhmm); runOnUiThread(onChanged); }).start();
                        })
                        .setNeutralButton("Any time", (d, w) ->
                                new Thread(() -> { daemonCmd("SET-OTA-TIME\n"); runOnUiThread(onChanged); }).start())
                        .setNegativeButton("Cancel", null)
                        .show();
            });
        }).start();
    }

    /** Dialog to pick how often the live session's data is sealed to the store (DIA-20260619-11). Presets
     *  only; the agent clamps anything odd to [15s, 1 day]. */
    private void showSyncIntervalPicker(Runnable onChanged) {
        final int[] secs = {30, 60, 120, 300, 600};
        final String[] labels = {"Every 30 seconds", "Every minute", "Every 2 minutes (default)",
                "Every 5 minutes", "Every 10 minutes"};
        new android.app.AlertDialog.Builder(this)
                .setTitle("Back up my data")
                .setItems(labels, (d, w) ->
                        new Thread(() -> { daemonCmd("SET-SYNC-INTERVAL\n" + secs[w]); runOnUiThread(onChanged); }).start())
                .setNegativeButton("Cancel", null)
                .show();
    }

    /** "every 30 seconds" / "every minute" / "every 2 minutes" for the backup-frequency row. */
    private static String prettyInterval(int sec) {
        if (sec % 60 == 0) {
            int m = sec / 60;
            return m == 1 ? "every minute" : "every " + m + " minutes";
        }
        return "every " + sec + " seconds";
    }

    /** Kiosk reliability: Lock Task can only be started from the FOREGROUND task, so if the gate lost the
     *  boot foreground race (e.g. SetupWizard came up over it), the onResume/onCreate lockGate(true) fails
     *  silently (state stays NONE) and the kiosk never engages. This fires when the gate actually gains the
     *  window focus -- i.e. it IS now foreground -- so retry the lock then. Idempotent: a no-op once LOCKED. */
    @Override
    public void onWindowFocusChanged(boolean hasFocus) {
        super.onWindowFocusChanged(hasFocus);
        if (hasFocus && wantLocked) {
            try {
                int st = ((android.app.ActivityManager) getSystemService(ACTIVITY_SERVICE)).getLockTaskModeState();
                if (st != android.app.ActivityManager.LOCK_TASK_MODE_LOCKED) {
                    android.util.Log.i("NowhereChooser", "gained focus + not locked -> retry lockGate");
                    lockGate(true);
                }
            } catch (Throwable ignore) {
            }
        }
    }

    /** During a no-reboot logoff the gate is switched to the foreground (phase 1) while the seal+remove are
     *  still finishing (phase 2) -- the form would otherwise look frozen. Reflect that: "Signing out…" +
     *  disabled Unlock + screen kept on, until the reap watcher clears `signingOut` and calls this again. */
    private void applySigningOutUi() {
        try {
            if (signingOut) {
                if (result != null) { result.setTextColor(MUTED); result.setText("Signing out…"); }
                if (unlockBtn != null) unlockBtn.setEnabled(false);
                getWindow().addFlags(android.view.WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
            } else {
                if (unlockBtn != null) unlockBtn.setEnabled(true);
                if (result != null && "Signing out…".contentEquals(result.getText())) result.setText("");
                getWindow().clearFlags(android.view.WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
            }
        } catch (Throwable ignore) {
        }
    }

    @Override
    public void onBackPressed() {
        // No escape from the gate.
    }

    @Override
    public boolean dispatchKeyEvent(KeyEvent event) {
        // Swallow the volume keys at the gate. It is a kiosk (they should do nothing), and -- critically --
        // letting them reach PhoneWindow's default handling CRASHES the gate: our confined nowhere_chooser
        // domain has no MediaSessionManager service, so dispatchVolumeKeyEvent NPEs on a null ISessionManager.
        // The crash drops Lock Task (kiosk) and lets the shade/notifications through. Consume them here.
        int kc = event.getKeyCode();
        if (kc == KeyEvent.KEYCODE_VOLUME_UP || kc == KeyEvent.KEYCODE_VOLUME_DOWN
                || kc == KeyEvent.KEYCODE_VOLUME_MUTE) {
            return true;
        }
        return super.dispatchKeyEvent(event);
    }

    /** Kiosk-lock the gate. Lock Task (device-owner) blocks the overview/home/back gestures + shade --
     *  the real fix for gesture nav; the StatusBarManager disable is a belt-and-suspenders fallback. */
    private void lockGate(boolean lock) {
        wantLocked = lock; // remember intent; onWindowFocusChanged + the timer below retry if it didn't engage
        lockHandler.removeCallbacks(lockRetry); // cancel any pending retry; re-armed at the end if still needed
        try {
            android.app.admin.DevicePolicyManager dpm =
                    (android.app.admin.DevicePolicyManager) getSystemService(Context.DEVICE_POLICY_SERVICE);
            boolean owner = dpm != null && dpm.isDeviceOwnerApp(getPackageName());
            android.util.Log.i("NowhereChooser", "lockGate(" + lock + ") owner=" + owner);
            if (owner) {
                if (lock) {
                    dpm.setLockTaskPackages(new android.content.ComponentName(this, AdminReceiver.class),
                            new String[]{getPackageName(), "com.android.phone", "com.android.emergency"}); // emergency dialer + info card at the locked gate
                    // Show the status-bar system info (Wi-Fi/signal/battery/clock) in the kiosk, so a user can
                    // SEE whether they're online at the gate -- before this, Lock Task's default (GLOBAL_ACTIONS
                    // only) suppressed the whole status bar (DIA-20260623-02). Keep GLOBAL_ACTIONS OR'd in: that's
                    // the power menu == the amnesiac power-off, and setLockTaskFeatures REPLACES the set, so
                    // omitting it would silently disable shutdown. NOT NOTIFICATIONS -> the shade still can't be
                    // pulled down, so no kiosk-escape surface is added.
                    dpm.setLockTaskFeatures(new android.content.ComponentName(this, AdminReceiver.class),
                            android.app.admin.DevicePolicyManager.LOCK_TASK_FEATURE_SYSTEM_INFO
                                    | android.app.admin.DevicePolicyManager.LOCK_TASK_FEATURE_GLOBAL_ACTIONS);
                    startLockTask();
                    int st = ((android.app.ActivityManager) getSystemService(ACTIVITY_SERVICE)).getLockTaskModeState();
                    android.util.Log.i("NowhereChooser", "startLockTask -> state=" + st);
                    // startLockTask throws/no-ops if our task isn't foreground (e.g. SetupWizard won the boot
                    // race -> "Invalid task, not in foreground"). If it didn't engage we DON'T give up: the
                    // retry fires from onWindowFocusChanged once the gate actually becomes the focused task.
                    if (st != android.app.ActivityManager.LOCK_TASK_MODE_LOCKED) {
                        android.util.Log.w("NowhereChooser", "lock not engaged (task not foreground?) -> will retry on focus");
                    }
                } else {
                    stopLockTask();
                }
            }
        } catch (Exception e) {
            android.util.Log.w("NowhereChooser", "lockGate err", e);
        }
        try {
            android.app.StatusBarManager sb =
                    (android.app.StatusBarManager) getSystemService("statusbar");
            sb.disable(lock
                    ? (android.app.StatusBarManager.DISABLE_EXPAND
                       | android.app.StatusBarManager.DISABLE_RECENT
                       | android.app.StatusBarManager.DISABLE_HOME
                       | android.app.StatusBarManager.DISABLE_BACK)
                    : android.app.StatusBarManager.DISABLE_NONE);
        } catch (Exception ignore) {
        }
        // If we want to be locked but aren't yet, retry on a timer. The big case: on a fresh /data the
        // first-boot provision sets device-owner LATE (the device-policy service isn't ready at boot), so
        // lockGate runs here with owner=false and does nothing -- and onWindowFocusChanged won't fire again
        // (the gate already has focus), so without this the kiosk stays down until a reboot. Bounded (~60s)
        // so it can't spin forever if device-owner never lands; once LOCKED we stop and reset.
        if (lock) {
            boolean locked = false;
            try {
                locked = ((android.app.ActivityManager) getSystemService(ACTIVITY_SERVICE)).getLockTaskModeState()
                        == android.app.ActivityManager.LOCK_TASK_MODE_LOCKED;
            } catch (Throwable ignore) {
            }
            if (locked) {
                lockAttempts = 0;
            } else if (lockAttempts < 30) {
                lockAttempts++;
                lockHandler.postDelayed(lockRetry, 2000);
            }
        } else {
            lockAttempts = 0;
        }
    }

    private int dp(float v) {
        return Math.round(v * getResources().getDisplayMetrics().density);
    }

    private EditText field(String hint, boolean password) {
        EditText e = new EditText(this);
        e.setHint(hint);
        e.setSingleLine(true);
        e.setTextColor(FIELD_TX);
        e.setHintTextColor(HINT);
        e.setTextSize(TypedValue.COMPLEX_UNIT_SP, 15);
        e.setInputType(password
                ? (InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_PASSWORD)
                : InputType.TYPE_CLASS_TEXT);
        GradientDrawable g = new GradientDrawable();
        g.setColor(FIELD_BG);
        g.setCornerRadius(dp(13));
        g.setStroke(dp(1), FIELD_LN);
        e.setBackground(g);
        e.setPadding(dp(15), dp(14), dp(15), dp(14));
        return e;
    }

    private LinearLayout.LayoutParams fieldLp(int bottom) {
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(MATCH, WRAP);
        lp.bottomMargin = bottom;
        return lp;
    }

    /** Send a one-line command to the login daemon and return its first reply line ("" on any error). #77:
     *  bounded connect via DaemonSocket so a wedged daemon can't hang this call. OTA-STATUS hits the store (a
     *  network GET), so allow more than a local probe for the read. */
    private String daemonCmd(String cmd) {
        return DaemonSocket.roundTrip(cmd + "\n", 8000);
    }

    /** Computed se_secrets, cached per identity. The value is DETERMINISTIC (HMAC of the persistent StrongBox key
     *  + name), so once computed it never changes within this (persistent, -800) process -- caching keeps it
     *  consistent and avoids re-hitting StrongBox on every roam op (fewer chances for a transient SE failure). */
    private final java.util.Map<String,String> seCache = new java.util.concurrent.ConcurrentHashMap<>();

    /** Endospore E.3b: a stable per-identity se_secret from a StrongBox-backed HMAC key (Titan M2). The key is
     *  non-exportable and lives in the secure element, so se_secret can only be computed ON this device -- a
     *  captured store ciphertext is then not offline-brute-forceable by passphrase alone. The key is created
     *  once and persists in user 0's keystore (which survives the amnesiac power-off, like the OS).
     *
     *  Returns "" ONLY when StrongBox is genuinely absent (e.g. the FP3) -> the identity stays a portable,
     *  pass-only vault. CRITICALLY, a *transient* StrongBox failure must NOT collapse to "" (DIA-20260621-06):
     *  StrongBox/Titan ops can fail/throttle transiently, and a "" at ROAM-IN means the HARDENED vault can't be
     *  opened -> empty restore + an empty se_secret in .roamsession -> the logoff/seal then fails "no slot opens"
     *  -> the session's changes (home layout, installed apps) silently never persist. So: distinguish "no SE at
     *  all" (the key can't be created -> "") from "SE present but the op threw" (RETRY the deterministic HMAC),
     *  and cache the result. */
    private String seSecretHex(String name) {
        String cached = seCache.get(name);
        if (cached != null) return cached;
        java.security.KeyStore ks;
        final String alias = "nowhere_se";
        try {
            ks = java.security.KeyStore.getInstance("AndroidKeyStore");
            ks.load(null);
            if (!ks.containsAlias(alias)) {
                javax.crypto.KeyGenerator kg = javax.crypto.KeyGenerator.getInstance(
                        android.security.keystore.KeyProperties.KEY_ALGORITHM_HMAC_SHA256, "AndroidKeyStore");
                kg.init(new android.security.keystore.KeyGenParameterSpec.Builder(
                        alias, android.security.keystore.KeyProperties.PURPOSE_SIGN)
                        .setIsStrongBoxBacked(true) // Titan M2; throws StrongBoxUnavailableException if absent
                        .build());
                kg.generateKey();
            }
        } catch (Throwable t) {
            // StrongBox genuinely absent (FP3) or keystore unavailable -> portable pass-only vault. The ONE legit "".
            android.util.Log.i("NowhereChooser", "seSecretHex: no StrongBox -> portable pass-only vault (" + t.getClass().getSimpleName() + ")");
            return "";
        }
        // The StrongBox key EXISTS -> this device is hardened. The HMAC is deterministic; retry on a transient SE
        // failure rather than returning "" (which would orphan the hardened vault).
        Throwable last = null;
        for (int attempt = 0; attempt < 6; attempt++) {
            try {
                javax.crypto.Mac mac = javax.crypto.Mac.getInstance("HmacSHA256");
                mac.init((javax.crypto.SecretKey) ks.getKey(alias, null));
                byte[] out = mac.doFinal(("nowhere-se:" + name).getBytes("UTF-8"));
                StringBuilder sb = new StringBuilder(out.length * 2);
                for (byte b : out) sb.append(String.format("%02x", b));
                String hex = sb.toString();
                seCache.put(name, hex);
                return hex;
            } catch (Throwable t) {
                last = t;
                android.util.Log.w("NowhereChooser", "seSecretHex HMAC attempt " + attempt + " failed (StrongBox key present, retrying): " + t);
                try { Thread.sleep(120L * (attempt + 1)); } catch (InterruptedException ignored) {}
            }
        }
        // Key present but the SE op kept failing -> a real fault, NOT the FP3 portable case. Don't cache; log
        // LOUDLY (returning "" would orphan the hardened vault). Rare; the retries above absorb normal transients.
        android.util.Log.e("NowhereChooser", "seSecretHex: StrongBox key present but HMAC FAILED after retries -- hardened vault will not open this pass", last);
        return "";
    }

    /** Ask the daemon whether a session is active (the tmpfs roaming state is populated). */
    private String queryStatus() {
        return daemonCmd("STATUS");
    }

    /** The system's CURRENT foreground user id (ActivityManager.getCurrentUser; the gate is device-owner with
     *  MANAGE_USERS). Used to gate the kiosk drop: finishing the gate is only safe when a SECONDARY user is
     *  foreground. On any error return 0 -- the SAFE default (treat as "user 0 is foreground" so the gate stays
     *  kiosk-locked rather than risk stranding on launcher3). */
    private int currentForegroundUser() {
        try {
            return android.app.ActivityManager.getCurrentUser();
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "getCurrentUser", t);
            return 0;
        }
    }

    /** Self-service OTA (DIA-20260618-03/04): ask the daemon if a newer OS is published (OTA-STATUS runs the
     *  version check in-process) and accent-tint the Settings gear if so, so an update stays discoverable on
     *  the gate without a footer link. The Install action itself lives in the Settings screen. Network call,
     *  so off the main thread; re-run on each onResume (e.g. after the user joins Wi-Fi). */
    private void checkForOta() {
        new Thread(() -> {
            final boolean avail = daemonCmd("OTA-STATUS").startsWith("AVAIL");
            runOnUiThread(() -> { if (gear != null) gear.setTextColor(avail ? ACCENT : MUTED); });
        }).start();
    }

    /** A tappable Settings row: a label + subtitle that opens an in-app sub-screen (stays in Lock-Task). */
    private View settingsRow(String label, String subtitle, View.OnClickListener onClick) {
        LinearLayout row = new LinearLayout(this);
        row.setOrientation(LinearLayout.VERTICAL);
        row.setPadding(dp(4), dp(13), dp(4), dp(13));
        TextView l = new TextView(this);
        l.setText(label);
        l.setTextColor(INK);
        l.setTextSize(TypedValue.COMPLEX_UNIT_SP, 16);
        row.addView(l);
        TextView s = new TextView(this);
        s.setText(subtitle);
        s.setTextColor(MUTED);
        s.setTextSize(TypedValue.COMPLEX_UNIT_SP, 12);
        row.addView(s);
        row.setOnClickListener(onClick);
        return row;
    }

    /** The Settings screen behind the gate's gear (DIA-20260618-04): device-config that used to clutter the
     *  gate footer — Store settings, Wi-Fi, and Software update. An in-app setContentView swap (like the
     *  Store/Wi-Fi/Recover screens), so it stays inside Lock-Task — no new activity, no kiosk escape. */
    private View buildSettingsScreen() {
        LinearLayout ll = new LinearLayout(this);
        ll.setOrientation(LinearLayout.VERTICAL);
        ll.setGravity(Gravity.CENTER_HORIZONTAL);
        ll.setBackgroundColor(BG);
        ll.setPadding(dp(28), dp(52), dp(28), dp(36));

        TextView t = new TextView(this);
        t.setText("Settings");
        t.setTextColor(INK);
        t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 26);
        t.setGravity(Gravity.CENTER);
        ll.addView(t);

        TextView sub = new TextView(this);
        sub.setText("Device configuration");
        sub.setTextColor(MUTED);
        sub.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        sub.setGravity(Gravity.CENTER);
        LinearLayout.LayoutParams slp = new LinearLayout.LayoutParams(MATCH, WRAP);
        slp.topMargin = dp(8);
        slp.bottomMargin = dp(24);
        ll.addView(sub, slp);

        ll.addView(settingsRow("Store settings", "Where your encrypted data is saved",
                v -> setContentView(buildStoreScreen())), new LinearLayout.LayoutParams(MATCH, WRAP));
        ll.addView(settingsRow("Wi-Fi", "Join a network",
                v -> setContentView(buildWifiScreen())), new LinearLayout.LayoutParams(MATCH, WRAP));

        // Backup frequency (DIA-20260619-11): how often the live session's data is sealed to the store. Lower
        // = less lost on an unclean power-off (crash / dead battery); convergent dedup keeps frequent cycles
        // cheap. Read/written via the daemon's GET/SET-SYNC-INTERVAL; syncLoop applies the change live.
        TextView bkHdr = new TextView(this);
        bkHdr.setText("Backup");
        bkHdr.setTextColor(INK);
        bkHdr.setTextSize(TypedValue.COMPLEX_UNIT_SP, 16);
        LinearLayout.LayoutParams bhp = new LinearLayout.LayoutParams(MATCH, WRAP);
        bhp.topMargin = dp(22);
        ll.addView(bkHdr, bhp);

        final TextView bkRow = new TextView(this);
        bkRow.setTextColor(ACCENT);
        bkRow.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        bkRow.setPadding(dp(2), dp(8), dp(2), dp(8));
        ll.addView(bkRow, new LinearLayout.LayoutParams(MATCH, WRAP));
        final Runnable refreshBkRow = () -> new Thread(() -> {
            int sec = 120;
            try { sec = Integer.parseInt(daemonCmd("GET-SYNC-INTERVAL").trim()); } catch (Exception e) { /* default */ }
            final int fsec = sec;
            runOnUiThread(() -> bkRow.setText("Your data is backed up " + prettyInterval(fsec) + "  ·  tap to change"));
        }).start();
        refreshBkRow.run();
        bkRow.setOnClickListener(v -> showSyncIntervalPicker(refreshBkRow));

        // Software update: a manual check + (when one is published) the user-confirm Install. The actual
        // download/apply/reboot is the su:s0 updater behind the daemon's OTA-APPLY (DIA-20260618-03).
        TextView swHdr = new TextView(this);
        swHdr.setText("Software update");
        swHdr.setTextColor(INK);
        swHdr.setTextSize(TypedValue.COMPLEX_UNIT_SP, 16);
        LinearLayout.LayoutParams shp = new LinearLayout.LayoutParams(MATCH, WRAP);
        shp.topMargin = dp(22);
        ll.addView(swHdr, shp);

        final TextView swMsg = new TextView(this);
        swMsg.setTextColor(MUTED);
        swMsg.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        LinearLayout.LayoutParams smp = new LinearLayout.LayoutParams(MATCH, WRAP);
        smp.topMargin = dp(4);
        ll.addView(swMsg, smp);

        // "Check for updates" stays anchored right under the status; the Install button appears BELOW it,
        // only when an update is published (View.GONE collapses -- no reserved gap), so the check link never
        // jumps as the button shows/hides.
        final TextView check = new TextView(this);
        check.setText("Check for updates");
        check.setTextColor(ACCENT);
        check.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
        check.setGravity(Gravity.START); // left-align under "Software update"
        check.setPadding(0, dp(12), dp(8), dp(8));
        ll.addView(check, new LinearLayout.LayoutParams(MATCH, WRAP));

        final Button install = accentButton("Install update");
        install.setVisibility(View.GONE);
        install.setPadding(dp(18), dp(14), dp(18), dp(14)); // accentButton has 0 horizontal padding; widen to ~"Check for updates"
        LinearLayout.LayoutParams ilp = new LinearLayout.LayoutParams(WRAP, WRAP); // sized to text+padding, not full width
        ilp.topMargin = dp(12);
        ilp.gravity = Gravity.START; // left-align, below Check for updates
        ll.addView(install, ilp);

        // Opt-in auto-install (DIA-20260618-06): when on, the gate installs a published update while charging
        // -- never mid-use. The flag is device-level (/data/nowhere/ota-auto via the daemon), default off.
        final android.widget.Switch autoSw = new android.widget.Switch(this);
        autoSw.setText("Install automatically while charging");
        autoSw.setTextColor(MUTED);
        autoSw.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        LinearLayout.LayoutParams asp = new LinearLayout.LayoutParams(MATCH, WRAP);
        asp.topMargin = dp(20);
        ll.addView(autoSw, asp);

        // Optional preferred install TIME (DIA-20260619-01): set -> install around that local time while
        // charging; "Any time" (the default) -> install at the next screen-off while charging. Only shown
        // when the toggle above is on.
        final TextView timeRow = new TextView(this);
        timeRow.setTextColor(ACCENT);
        timeRow.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        timeRow.setPadding(dp(2), dp(8), dp(2), dp(8));
        timeRow.setVisibility(View.GONE);
        ll.addView(timeRow, new LinearLayout.LayoutParams(MATCH, WRAP));
        final Runnable refreshTimeRow = () -> new Thread(() -> {
            final String tv = daemonCmd("GET-OTA-TIME");
            runOnUiThread(() -> timeRow.setText(tv.isEmpty()
                    ? "Install time:  Any time  ·  tap to set"
                    : "Install time:  " + prettyTime(tv) + "  ·  tap to change"));
        }).start();
        timeRow.setOnClickListener(v -> showOtaTimePicker(refreshTimeRow));

        new Thread(() -> {
            final boolean on = "1".equals(daemonCmd("GET-OTA-AUTO"));
            runOnUiThread(() -> {
                autoSw.setChecked(on); // set state first, THEN wire the listener so loading doesn't write back
                timeRow.setVisibility(on ? View.VISIBLE : View.GONE);
                if (on) refreshTimeRow.run();
                autoSw.setOnCheckedChangeListener((b, c) -> {
                    timeRow.setVisibility(c ? View.VISIBLE : View.GONE);
                    if (c) refreshTimeRow.run();
                    new Thread(() -> daemonCmd("SET-OTA-AUTO\n" + (c ? "1" : "0"))).start();
                });
            });
        }).start();

        final Runnable doCheck = () -> {
            swMsg.setText("Checking…");
            install.setVisibility(View.GONE);
            new Thread(() -> {
                final String r = daemonCmd("OTA-STATUS");
                final boolean avail = r.startsWith("AVAIL");
                final String ver = avail && r.length() > 5 ? r.substring(5).trim() : "";
                runOnUiThread(() -> {
                    swMsg.setText(avail ? "Update available" + (ver.isEmpty() ? "" : " (" + ver + ")")
                            : (r.isEmpty() ? "Couldn't check — is Wi-Fi on?" : "Up to date"));
                    install.setVisibility(avail ? View.VISIBLE : View.GONE);
                });
            }).start();
        };
        check.setOnClickListener(v -> doCheck.run());
        install.setOnClickListener(v -> {
            install.setVisibility(View.GONE);
            check.setVisibility(View.GONE);
            swMsg.setTextColor(ACCENT);
            swMsg.setText("Updating… the phone will restart when it's done.");
            new Thread(() -> daemonCmd("OTA-APPLY")).start();
        });

        TextView back = new TextView(this);
        back.setText("Back");
        back.setTextColor(ACCENT);
        back.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
        back.setGravity(Gravity.CENTER);
        back.setPadding(dp(8), dp(24), dp(8), dp(8));
        ll.addView(back, new LinearLayout.LayoutParams(MATCH, WRAP));
        back.setOnClickListener(v -> setContentView(gateView));

        doCheck.run(); // auto-check on open

        android.widget.ScrollView sv = new android.widget.ScrollView(this);
        sv.setBackgroundColor(BG);
        sv.setFillViewport(true);
        sv.addView(ll);
        return sv;
    }

    /** Hand off to the real launcher -- the HOME activity that isn't us. */
    private void launchLauncher() {
        Intent home = new Intent(Intent.ACTION_MAIN).addCategory(Intent.CATEGORY_HOME);
        List<ResolveInfo> ris = getPackageManager().queryIntentActivities(home, 0);
        for (ResolveInfo ri : ris) {
            String pkg = ri.activityInfo.packageName;
            // Skip ourselves and the system fallback home (com.android.settings/.FallbackHome -- it
            // requires DEVICE_POWER and isn't a real launcher); start the first one that accepts it.
            if (getPackageName().equals(pkg) || "com.android.settings".equals(pkg)) {
                continue;
            }
            try {
                startActivity(new Intent(Intent.ACTION_MAIN)
                        .addCategory(Intent.CATEGORY_HOME)
                        .setClassName(pkg, ri.activityInfo.name)
                        .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK));
                android.util.Log.i("NowhereChooser", "handoff -> " + pkg + "/" + ri.activityInfo.name);
                return;
            } catch (Exception e) {
                android.util.Log.w("NowhereChooser", "home not launchable: " + pkg + " -> " + e);
            }
        }
        android.util.Log.w("NowhereChooser", "no launcher to hand off to (" + ris.size() + " home candidates)");
    }

    /** P4: switch the gate into "Welcome back &lt;name&gt;" resume mode for a cold-locked session -- passphrase
     *  only (the name is known), a Resume button, no name/create field. Idempotent (safe to call on every
     *  onResume); only re-toggles the views the first time so it never steals focus mid-typing. */
    private void enterResumeMode(String name, String uid) {
        resumeName = name; resumeUid = uid;
        if (resumeMode) return; // already showing -> just keep name/uid fresh, don't re-toggle / refocus
        resumeMode = true;
        createMode = false;
        confirmField.setVisibility(View.GONE);
        profileField.setVisibility(View.GONE);
        profileField.setText("");
        toggle.setVisibility(View.GONE);
        footerView.setVisibility(View.GONE);
        resumeAlt.setVisibility(View.VISIBLE);
        subTitle.setText("Welcome back, " + name);
        unlockBtn.setText("Resume");
        result.setText("");
        passField.setText("");
        passField.requestFocus();
    }

    /** P4: drop resume mode back to the standard fresh-login gate -- "not you?" tapped, or the cold-lock is
     *  gone (resumed/wiped elsewhere). The cold-locked user, if any, lingers until a logoff/boot-wipe reaps it. */
    private void exitResumeMode() {
        if (!resumeMode) return;
        resumeMode = false;
        resumeName = null; resumeUid = null;
        profileField.setVisibility(View.VISIBLE);
        toggle.setVisibility(View.VISIBLE);
        footerView.setVisibility(View.VISIBLE);
        resumeAlt.setVisibility(View.GONE);
        subTitle.setText("your phone, nowhere");
        unlockBtn.setText("Unlock");
        result.setText("");
        passField.setText("");
        profileField.requestFocus();
    }

    /** P4: resume a cold-locked session. The daemon verifies the typed passphrase against the user's cold CE
     *  (decrypting it in place -- verify BEFORE the switch, the recipe that avoids the disabled-keyguard crash)
     *  and re-arms the session marker; on OK we switch straight in -- NO restore, the data was never removed.
     *  A wrong pass leaves the CE cold. Mirrors roamLogin's switch-in tail (welcome message, sessionLive, idle
     *  watcher) without the create/restore half. */
    private void resume(final String pass) {
        if (pass == null || pass.isEmpty()) { result.setTextColor(MUTED); result.setText("enter your passphrase"); return; }
        if (resumeBusy) return;
        resumeBusy = true;
        unlockBtn.setEnabled(false);
        result.setTextColor(MUTED); result.setText("Resuming…");
        final android.content.Context appCtx = getApplicationContext();
        final String nm = resumeName;
        new Thread(() -> {
            String reply = sendDaemon("RESUME\n" + pass + "\n").trim();
            if (reply.startsWith("OK")) {
                int u = -1;
                try { u = Integer.parseInt(reply.substring(2).trim()); } catch (Exception ignore) {}
                final int fuid = u;
                // Welcome-back on the user-switch overlay (where the eyes are during the switch), like login.
                try {
                    android.app.admin.DevicePolicyManager dpm = (android.app.admin.DevicePolicyManager) getSystemService(DEVICE_POLICY_SERVICE);
                    android.content.ComponentName dpmAdmin = new android.content.ComponentName(this, AdminReceiver.class);
                    dpm.setStartUserSessionMessage(dpmAdmin, "Welcome back, " + nm);
                    dpm.setEndUserSessionMessage(dpmAdmin, "Signing off…");
                } catch (Throwable t) { android.util.Log.w("NowhereChooser", "resume welcome msg", t); }
                if (fuid >= 0) {
                    sessionLive = true; // a session is live again -> the reap watcher polls fast for the next logoff
                    // Switch FIRST; only drop the kiosk + finish if it actually took. A failed switch with the
                    // kiosk dropped would strand on user 0's launcher3 (the same escape the onResume guard closes).
                    runOnUiThread(() -> {
                        if (switchToUser(fuid)) {
                            resumeMode = false; lockGate(false); finish();
                        } else {
                            sessionLive = false; resumeBusy = false; unlockBtn.setEnabled(true);
                            result.setTextColor(MUTED); result.setText("couldn't resume — try again");
                        }
                    });
                    // The RESUME op CLEARED the session credential (so the switch lands on home, one passphrase
                    // -- not a second Android keyguard prompt). Re-arm it for the next screen-off, exactly like a
                    // fresh login, so the warm L1 keyguard + the next cold-lock still work. (DIA-20260625-13)
                    armCredentialOnScreenOff(appCtx, fuid, pass);
                    armIdleWatcher(appCtx, fuid); // re-arm L4 idle cold-lock for the resumed session
                } else {
                    runOnUiThread(() -> { resumeBusy = false; unlockBtn.setEnabled(true); result.setText("couldn't resume — try again"); });
                }
            } else {
                final String r = reply;
                runOnUiThread(() -> {
                    resumeBusy = false;
                    unlockBtn.setEnabled(true);
                    passField.setText("");
                    result.setTextColor(MUTED);
                    if (r.startsWith("NONE")) { result.setText(""); exitResumeMode(); } // cold-lock vanished -> fresh gate
                    else result.setText("wrong passphrase");
                });
            }
        }).start();
    }

    /** L1: arm the session keyguard credential for the FIRST screen-off -- a one-shot ACTION_SCREEN_OFF receiver
     *  that sets user &lt;uid&gt;'s lockscreen credential to &lt;pw&gt; while the screen is already off, so login/resume
     *  land on HOME (not a lockscreen) and the keyguard only appears once the user turns the screen off. Replaces
     *  any prior armer. Used by roamLogin (new login) AND resume (a cold-locked session whose credential the
     *  resume just CLEARED, to avoid the second passphrase). (DIA-20260623-13 / DIA-20260625-13) */
    private static void armCredentialOnScreenOff(final android.content.Context appCtx, final int uid, final String pw) {
        synchronized (ChooserActivity.class) {
            if (sCredArmer != null) { try { appCtx.unregisterReceiver(sCredArmer); } catch (Throwable ignore) {} sCredArmer = null; }
            sCredArmer = new android.content.BroadcastReceiver() {
                @Override public void onReceive(android.content.Context c, android.content.Intent i) {
                    synchronized (ChooserActivity.class) {
                        try { appCtx.unregisterReceiver(this); } catch (Throwable ignore) {}
                        if (sCredArmer == this) sCredArmer = null;
                    }
                    try {
                        appCtx.startServiceAsUser(new android.content.Intent(appCtx, CredentialService.class)
                                .putExtra("pw", pw), android.os.UserHandle.of(uid));
                    } catch (Throwable t) { android.util.Log.w("NowhereChooser", "screen-off CredentialService", t); }
                }
            };
            try {
                appCtx.registerReceiver(sCredArmer,
                        new android.content.IntentFilter(android.content.Intent.ACTION_SCREEN_OFF));
            } catch (Throwable t) { android.util.Log.w("NowhereChooser", "register cred armer", t); sCredArmer = null; }
        }
    }

    private void unlock(final String name, final String pw) {
        roamLogin(name, pw, false);
    }

    /** Enroll a new identity (establish the profile in the store), then roam straight into it. */
    private void create(final String name, final String pw) {
        roamLogin(name, pw, true);
    }

    /**
     * Arc 2 login. The IDENTITY is a fresh EPHEMERAL Android user: create+start it (device-owner DPM),
     * have the root daemon roam the profile's /data/user/N data into it (ROAM-IN -> su:s0 worker), then
     * switch in. Blind: a wrong cred / unknown profile yields BLANK -> uniform "—", and the throwaway
     * user is stopped (the gate on user 0 reaps it). For create, the profile is established first so the
     * subsequent roam-in succeeds. Ephemeral => power-off or logout wipes the user; nothing stays.
     */
    private void roamLogin(final String name, final String pw, final boolean isCreate) {
        new Thread(() -> {
          loginInProgress = true; // #73: hold off the gate's orphan-sweep from the instant we may create a user
          try {
            if (!hasInternet()) { // a restore needs the store -> don't attempt a doomed one that just times
                // out to a cryptic blank; tell the user to connect (covers "no wifi" AND "wifi still rejoining
                // after a reboot"). Reveals only network state, not whether a profile exists -> deniability kept.
                showMsg("No internet yet — tap Wi-Fi to connect (or wait for it to reconnect)");
                return;
            }
            showRestoring(name); // swap the gate to "Logging in <name>…" + the progress bar
            if (isCreate) {
                String cr = sendDaemon("CREATE\n" + name + "\n" + pw + "\n");
                if (cr.startsWith("EXISTS")) { hideRestore(); showMsg("that profile already exists"); return; }
                if (cr.startsWith("NOSTORE")) { hideRestore(); showStoreHint(); return; } // set up a store first
                if (cr.startsWith("RATELIMIT")) { hideRestore(); showMsg(rateLimitMsg(cr)); return; } // enrollment throttled
                if (cr.startsWith("NOCREDIT")) { // managed store + empty wallet: an honest message, not the blind "—"
                    hideRestore();
                    showMsg("Saved profiles need storage credit. Sign in to use a throwaway for now (free), then save it to your store later.");
                    return;
                }
                if (!cr.startsWith("OK")) { hideRestore(); showBlank(); return; }
                String rec = parseRecovery(cr); // "OK 0 RECOVERY <12 words>" -> show the code once, then continue
                if (rec != null) { showRecoveryCode(rec); showRestoring(name); }
                // Endospore E.3b: harden the brand-new identity to this device's secure element -- adds the
                // device `se` keyslot + drops the pass-only slot (no longer offline-brute-forceable). On a
                // device without StrongBox (FP3) seSecretHex returns "" -> stays a portable pass-only vault.
                String se = seSecretHex(name);
                if (!se.isEmpty()) sendDaemon("ENROLL-SE\n" + name + "\n" + pw + "\n" + se + "\n");
            }
            sawRestoreProgress = false; // reset per login attempt (#70/#75: connection-vs-blind message)
            int uid = createRoamUser(name);
            // Quiesce the crash-prone apps BEFORE the restore: disabling stops them AND blocks relaunch, so
            // the restore can't swap data under their open DB handles (which is what crash-loops them).
            if (uid >= 0) setRoamCrashersEnabled(uid, false);
            String reply = uid < 0 ? "ERR-CREATEUSER"
                    : roamInStreaming(name, pw, uid); // streams PROGRESS -> the bar, returns the OK/BLANK verdict
            final boolean ok = reply.startsWith("OK");
            // result.txt keeps the verdict for the adb-driven test; the UI itself stays blind.
            try (FileWriter fw = new FileWriter(getFilesDir() + "/result.txt")) {
                fw.write("name=" + name + " uid=" + uid + " verdict=" + (ok ? "ROAMED" : "BLANK")
                        + " reply=" + reply + "\n");
            } catch (Exception ignore) {
            }
            if (ok) {
                String rec = parseRecovery(reply); // present iff this login auto-migrated an old profile to a vault
                if (rec != null) showRecoveryCode(rec); // "your profile was upgraded -- save this recovery code"
                runOnUiThread(() -> { progressBar.setProgress(100); result.setTextColor(ACCENT); result.setText("Finishing up…"); });
                prepRoamedUser(uid); // land clean: re-enable the quiesced apps + keep the setup wizard off
                provisionRoamedApps(uid); // reinstall the roamed identity's apps (device-local, for now)
                applyRoamedPrefs(); // re-apply this profile's roamed timezone + locale (or the baked default)
                // Drop the kiosk, switch into the roamed user, finish -- all on the UI thread, in order.
                sessionLive = true; // a session is now live -> the reap watcher polls fast for a quick logoff (DIA-20260618-07)
                // Welcome-back ON the Android user-switch screen (where the eyes are during the switch), via the
                // device-owner start-session message -- a returning login (data restored) shows the name + when
                // it was last active; a fresh/mistyped one has no LASTSEAL so it's just "Welcome back, <name>".
                // (DIA-20260625-05b -- moved off the gate.)
                try {
                    // Returning (data restored -> a LASTSEAL ts) gets "Welcome back, … · last active …"; a NEW
                    // profile (create, or no prior seal) just gets "Welcome, <name>" -- no "back". (DIA-20260625-05b)
                    String wb = (!isCreate && lastSealTs > 0)
                            ? "Welcome back, " + name + " · last active " + relTime(lastSealTs)
                            : "Welcome, " + name;
                    android.app.admin.DevicePolicyManager dpm = (android.app.admin.DevicePolicyManager) getSystemService(DEVICE_POLICY_SERVICE);
                    android.content.ComponentName dpmAdmin = new android.content.ComponentName(this, AdminReceiver.class);
                    dpm.setStartUserSessionMessage(dpmAdmin, wb);
                    // Symmetric to the welcome: the LOGOFF user-switch overlay (this user -> gate) shows
                    // "Signing off…" instead of the framework default ("One moment…"). Generic, set once here
                    // alongside the welcome; the END message fires when this session is stopped at reap. (DIA-20260625-05d)
                    dpm.setEndUserSessionMessage(dpmAdmin, "Signing off…");
                } catch (Throwable t) { android.util.Log.w("NowhereChooser", "setStartUserSessionMessage", t); }
                runOnUiThread(() -> { lockGate(false); switchToUser(uid); finish(); });
                // L1 (DIA-20260623-13): DON'T set the session credential now -- enrolling a lockscreen
                // credential LOCKS the screen, which would dump the user (who just authenticated at the gate)
                // straight onto a lockscreen (the "double passphrase" / brief-home-then-lock you saw). Instead
                // ARM it for the FIRST screen-off: set it while the screen is already off (so the lock is
                // invisible) -> login lands on HOME and the keyguard only appears once the user themselves
                // turns the screen off. resetPasswordWithToken ignores a cross-user context, so
                // CredentialService runs AS the roamed user (its profile owner) via startServiceAsUser. The
                // gate process is persistent and the receiver is app-context-registered, so it outlives
                // finish(); the passphrase rides the extra (binder / in-memory), never on disk.
                final int fuid = uid;
                final android.content.Context appCtx = getApplicationContext();
                armCredentialOnScreenOff(appCtx, fuid, pw); // L1: arm the keyguard credential for the first screen-off
                armIdleWatcher(appCtx, fuid); // L4 (DIA-20260623-17): screen-off arms an idle-logoff alarm
            } else {
                if (uid >= 0) stopRoamUser(uid); // blind: no access; gate/reboot reaps the empty user
                hideRestore();
                // Don't blame the CREDENTIALS for a CONNECTION failure (DIA-20260630-44, #70/#75). If we saw any
                // PROGRESS, the creds were accepted + the restore started -> a network/timeout issue, not a wrong
                // cred; reply=="" means our own socket timed out. Only an IMMEDIATE no-verdict (no progress) stays
                // blind ("check your name"), which is what a wrong cred / unknown profile looks like -- and this
                // can't leak, because a wrong cred never streams progress.
                if (reply.startsWith("NOSTORE")) showStoreHint();
                else if (reply.isEmpty() || sawRestoreProgress) {
                    // #58 P4: the creds were accepted and the restore STARTED but didn't finish -- usually a
                    // connection hiccup (retry), but it can be a damaged latest save (a corrupt store chunk), which
                    // a retry can't fix. Offer BOTH: try again, or roll back to the last good snapshot. (A brand-new
                    // create has no snapshots, so it keeps the plain message.)
                    if (isCreate) showMsg("Couldn't finish setting up — check your connection and try again.");
                    else offerSnapshotRecovery(name, pw);
                } else showBlank();
            }
          } finally {
            loginInProgress = false; // #73: login resolved (switched in on success, or user reaped on failure)
          }
        }).start();
    }

    /** #58 P4: after a login restore failed post-progress (connection hiccup OR a damaged latest save), offer a
     *  retry OR a rollback to the last good snapshot. */
    private void offerSnapshotRecovery(final String name, final String pw) {
        runOnUiThread(() -> new android.app.AlertDialog.Builder(this)
                .setTitle("Couldn't load your data")
                .setMessage("Your latest data didn't finish loading. This is usually a connection hiccup — try "
                        + "again. If it keeps failing, your most recent save may be damaged; you can restore your "
                        + "last good version instead.")
                .setPositiveButton("Try again", (d, w) -> roamLogin(name, pw, false))
                .setNeutralButton("Restore last good version", (d, w) -> doGateRollback(name, pw))
                .setNegativeButton("Cancel", (d, w) -> { hideRestore(); if (result != null) result.setText(""); })
                .show());
    }

    /** #58 P4: roll the store head back to the newest snapshot, then retry the login so it loads the last good head. */
    private void doGateRollback(final String name, final String pw) {
        showRestoring(name);
        new Thread(() -> {
            String r = sendDaemon("ROLLBACK-GATE\n" + name + "\n" + pw + "\n").trim();
            if (r.startsWith("OK")) {
                roamLogin(name, pw, false); // retry -> restores the rolled-back (last good) head
            } else {
                runOnUiThread(() -> {
                    hideRestore();
                    showMsg(r.startsWith("NOSNAP") ? "No earlier version available to restore."
                            : "Couldn't restore — check your connection and try again.");
                });
            }
        }).start();
    }

    /** Swap the gate into the restore-in-progress state: gray out + lock the whole form, dismiss the
     *  keyboard (so nothing can be typed/submitted mid-restore and the bar isn't hidden behind the IME),
     *  and show the bar at 0%. */
    private void showRestoring(final String name) {
        runOnUiThread(() -> {
            unlockBtn.setEnabled(false);
            profileField.setEnabled(false);
            passField.setEnabled(false);
            confirmField.setEnabled(false);
            toggle.setEnabled(false);
            // setEnabled alone won't gray these out -- their backgrounds are custom GradientDrawables with no
            // disabled state -- so dim them explicitly to read as inactive.
            float dim = 0.4f;
            profileField.setAlpha(dim);
            passField.setAlpha(dim);
            confirmField.setAlpha(dim);
            unlockBtn.setAlpha(dim);
            toggle.setAlpha(dim);
            hideKeyboard();
            progressBar.setProgress(0);
            progressBar.setVisibility(View.VISIBLE);
            result.setTextColor(ACCENT);
            result.setText("Logging in " + name + "…");
            // Restore can take 20-30s; don't let the screen sleep mid-login.
            getWindow().addFlags(android.view.WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
        });
    }

    /** Undo showRestoring (login failed/blank): hide the bar, re-enable the form. */
    private void hideRestore() {
        runOnUiThread(() -> {
            getWindow().clearFlags(android.view.WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
            progressBar.setVisibility(View.GONE);
            unlockBtn.setEnabled(true);
            profileField.setEnabled(true);
            passField.setEnabled(true);
            confirmField.setEnabled(true);
            toggle.setEnabled(true);
            profileField.setAlpha(1f);
            passField.setAlpha(1f);
            confirmField.setAlpha(1f);
            unlockBtn.setAlpha(1f);
            toggle.setAlpha(1f);
            result.setText("");
        });
    }

    /** Dismiss the soft keyboard while the restore takes over the gate. */
    private void hideKeyboard() {
        try {
            android.view.inputmethod.InputMethodManager imm =
                    (android.view.inputmethod.InputMethodManager) getSystemService(INPUT_METHOD_SERVICE);
            View f = getCurrentFocus();
            android.os.IBinder tok = (f != null) ? f.getWindowToken()
                    : getWindow().getDecorView().getWindowToken();
            if (imm != null && tok != null) imm.hideSoftInputFromWindow(tok, 0);
            if (f != null) f.clearFocus();
        } catch (Throwable ignore) {
        }
    }

    /** Pull the "<12 words>" out of a daemon reply's "... RECOVERY <words>" suffix, or null if absent. */
    private String parseRecovery(String reply) {
        int i = reply.indexOf(" RECOVERY ");
        return i < 0 ? null : reply.substring(i + " RECOVERY ".length()).trim();
    }

    /** The edition brand shown on the gate. Per-edition (`endospore` / `diaspore`), set via
     *  ro.nowhere.brand in the edition .mk; falls back to "nowhere" for the shared/default build. */
    private String brand() {
        try {
            String b = android.os.SystemProperties.get("ro.nowhere.brand", "nowhere");
            return (b == null || b.isEmpty()) ? "nowhere" : b;
        } catch (Throwable t) { return "nowhere"; }
    }

    /** Short provider name from a store endpoint URL for the store screen's connection banner, e.g.
     *  https://s3.filebase.com -> "filebase", https://s3.us-east-1.amazonaws.com -> "amazonaws". */
    private String providerFromEndpoint(String endpoint) {
        try {
            if (endpoint == null || endpoint.isEmpty()) return "store";
            String host = android.net.Uri.parse(endpoint).getHost();
            if (host == null || host.isEmpty()) return "store";
            String[] p = host.split("\\.");
            return p.length >= 2 ? p[p.length - 2] : host;
        } catch (Throwable t) { return "store"; }
    }

    /** The Nowhere accent button (rounded), matching the gate's Unlock. */
    private Button accentButton(String text) {
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

    /** Show the 12-word recovery code full-screen and BLOCK the login thread until the user confirms they've
     *  written it down. Shown once -- on create, and on the one-time auto-migration of a legacy profile. */
    private void showRecoveryCode(final String mnemonic) {
        final java.util.concurrent.CountDownLatch latch = new java.util.concurrent.CountDownLatch(1);
        runOnUiThread(() -> setContentView(buildRecoveryScreen(mnemonic, latch)));
        try { latch.await(); } catch (InterruptedException ignore) {}
    }

    private View buildRecoveryScreen(String mnemonic, final java.util.concurrent.CountDownLatch latch) {
        LinearLayout ll = new LinearLayout(this);
        ll.setOrientation(LinearLayout.VERTICAL);
        ll.setBackgroundColor(BG);
        ll.setPadding(dp(28), dp(52), dp(28), dp(36));

        TextView t = new TextView(this);
        t.setText("Your recovery code");
        t.setTextColor(INK);
        t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 24);
        t.setGravity(Gravity.CENTER);
        ll.addView(t);

        TextView warn = new TextView(this);
        warn.setText("Write these 12 words down and keep them somewhere safe. They are the ONLY way back into "
                + "your profile if you forget your passphrase -- no one, including us, can recover it for you.");
        warn.setTextColor(MUTED);
        warn.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        warn.setGravity(Gravity.CENTER);
        warn.setLineSpacing(dp(3), 1f);
        LinearLayout.LayoutParams wlp = new LinearLayout.LayoutParams(MATCH, WRAP);
        wlp.topMargin = dp(12);
        wlp.bottomMargin = dp(22);
        ll.addView(warn, wlp);

        TextView words = new TextView(this);
        String[] w = mnemonic.split("\\s+");
        StringBuilder sb = new StringBuilder();
        for (int i = 0; i < w.length; i += 2) {
            sb.append(String.format("%2d  %-11s", i + 1, w[i]));
            if (i + 1 < w.length) sb.append(String.format("%2d  %s", i + 2, w[i + 1]));
            if (i + 2 < w.length) sb.append("\n");
        }
        words.setText(sb.toString());
        words.setTextColor(ACCENT);
        words.setTextSize(TypedValue.COMPLEX_UNIT_SP, 16);
        words.setTypeface(android.graphics.Typeface.MONOSPACE);
        GradientDrawable wbg = new GradientDrawable();
        wbg.setColor(FIELD_BG);
        wbg.setCornerRadius(dp(13));
        wbg.setStroke(dp(1), FIELD_LN);
        words.setBackground(wbg);
        words.setPadding(dp(18), dp(20), dp(18), dp(20));
        LinearLayout.LayoutParams wdlp = new LinearLayout.LayoutParams(MATCH, WRAP);
        wdlp.bottomMargin = dp(26);
        ll.addView(words, wdlp);

        Button done = accentButton("I've written it down");
        ll.addView(done, new LinearLayout.LayoutParams(MATCH, WRAP));
        // Creating the Android user (createRoamUser: createAndManageUser + startUserInBackground + the initial
        // roamed-session setup) is genuinely heavy (a few seconds). Give immediate feedback so it doesn't read as
        // frozen, and disable the button so a second tap can't fire a second create (DIA-20260622).
        done.setOnClickListener(v -> {
            done.setEnabled(false);
            done.setText("Creating your space…");
            latch.countDown();
        });

        android.widget.ScrollView sc = new android.widget.ScrollView(this);
        sc.setBackgroundColor(BG);
        sc.addView(ll);
        return sc;
    }

    /** The "Forgot passphrase?" recover screen: name + 12-word code -> a new passphrase (daemon RECOVER). */
    private View buildRecoverScreen() {
        LinearLayout ll = new LinearLayout(this);
        ll.setOrientation(LinearLayout.VERTICAL);
        ll.setGravity(Gravity.CENTER_HORIZONTAL);
        ll.setBackgroundColor(BG);
        ll.setPadding(dp(28), dp(52), dp(28), dp(36));

        TextView t = new TextView(this);
        t.setText("Reset passphrase");
        t.setTextColor(INK);
        t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 26);
        t.setGravity(Gravity.CENTER);
        ll.addView(t);

        TextView sub = new TextView(this);
        sub.setText("Enter your profile name and your 12-word recovery code, then choose a new passphrase.");
        sub.setTextColor(MUTED);
        sub.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        sub.setGravity(Gravity.CENTER);
        sub.setLineSpacing(dp(3), 1f);
        LinearLayout.LayoutParams slp = new LinearLayout.LayoutParams(MATCH, WRAP);
        slp.topMargin = dp(10);
        slp.bottomMargin = dp(24);
        ll.addView(sub, slp);

        final EditText nameF = field("profile", false);
        ll.addView(nameF, fieldLp(dp(11)));
        final EditText phraseF = field("12-word recovery code", false);
        phraseF.setSingleLine(false);
        phraseF.setMinLines(2);
        ll.addView(phraseF, fieldLp(dp(11)));
        final EditText newP = field("new passphrase", true);
        ll.addView(newP, fieldLp(dp(20)));

        final Button reset = accentButton("Reset passphrase");
        ll.addView(reset, new LinearLayout.LayoutParams(MATCH, WRAP));

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
        cancel.setOnClickListener(v -> setContentView(gateView));

        // status/error message goes BELOW Cancel so it doesn't push the button + Cancel apart
        LinearLayout.LayoutParams mlp = new LinearLayout.LayoutParams(MATCH, WRAP);
        mlp.topMargin = dp(12);
        ll.addView(msg, mlp);

        reset.setOnClickListener(v -> {
            final String nm = nameF.getText().toString().trim();
            final String ph = phraseF.getText().toString().trim().replaceAll("\\s+", " ");
            final String np = newP.getText().toString();
            if (nm.isEmpty() || ph.isEmpty() || np.isEmpty()) {
                msg.setTextColor(MUTED);
                msg.setText("fill in all three fields");
                return;
            }
            reset.setEnabled(false);
            msg.setTextColor(ACCENT);
            msg.setText("resetting…");
            new Thread(() -> {
                String r = sendDaemon("RECOVER\n" + nm + "\n" + ph + "\n" + np + "\n");
                runOnUiThread(() -> {
                    if (r.startsWith("OK")) {
                        setContentView(gateView);
                        profileField.setText(nm);
                        passField.setText("");
                        result.setTextColor(ACCENT);
                        result.setText("passphrase reset — sign in with your new one");
                    } else {
                        reset.setEnabled(true);
                        msg.setTextColor(MUTED);
                        msg.setText("couldn't reset — check the name and recovery code"); // blind: don't say which
                    }
                });
            }).start();
        });

        android.widget.ScrollView sc = new android.widget.ScrollView(this);
        sc.setBackgroundColor(BG);
        sc.addView(ll);
        return sc;
    }

    /** Settings/Store: view + set WHERE this device saves data (an S3-compatible endpoint + creds). Writes
     *  via the daemon's SET-STORE (root, to /data/nowhere/nowhere.conf, applied live); GET-STORE pre-fills
     *  the non-secret fields. The store config is device-level -- it persists the power-off wipe. */
    private View buildStoreScreen() {
        LinearLayout ll = new LinearLayout(this);
        ll.setOrientation(LinearLayout.VERTICAL);
        ll.setGravity(Gravity.CENTER_HORIZONTAL);
        ll.setBackgroundColor(BG);
        ll.setPadding(dp(28), dp(52), dp(28), dp(36));

        TextView t = new TextView(this);
        t.setText("Store settings");
        t.setTextColor(INK);
        t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 26);
        t.setGravity(Gravity.CENTER);
        ll.addView(t);

        TextView sub = new TextView(this);
        sub.setText("Where your encrypted data is saved. Point this device at an S3-compatible store.");
        sub.setTextColor(MUTED);
        sub.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        sub.setGravity(Gravity.CENTER);
        sub.setLineSpacing(dp(3), 1f);
        LinearLayout.LayoutParams slp = new LinearLayout.LayoutParams(MATCH, WRAP);
        slp.topMargin = dp(10);
        slp.bottomMargin = dp(24);
        ll.addView(sub, slp);

        // Connection banner (DIA-20260623-33): mirror the Wi-Fi screen's "✓ Connected to <ssid>" -- show
        // "✓ Connected to <store>" here so the store screen confirms where this device's data is kept.
        // Populated from GET-STORE below; shown only when a store is configured.
        final TextView conn = new TextView(this);
        conn.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
        conn.setGravity(Gravity.CENTER);
        conn.setVisibility(View.GONE);
        LinearLayout.LayoutParams cnLp = new LinearLayout.LayoutParams(MATCH, WRAP);
        cnLp.bottomMargin = dp(18);
        ll.addView(conn, cnLp);

        final EditText endpointF = field("store URL (https://…)", false);
        ll.addView(endpointF, fieldLp(dp(11)));
        final EditText bucketF = field("bucket", false);
        ll.addView(bucketF, fieldLp(dp(11)));
        final EditText regionF = field("region (optional)", false);
        ll.addView(regionF, fieldLp(dp(11)));
        final EditText keyF = field("access key", false);
        ll.addView(keyF, fieldLp(dp(11)));
        final EditText secretF = field("secret key", true);
        ll.addView(secretF, fieldLp(dp(20)));

        final Button save = accentButton("Save store");
        ll.addView(save, new LinearLayout.LayoutParams(MATCH, WRAP));

        final TextView test = new TextView(this);
        test.setText("Test connection");
        test.setTextColor(ACCENT);
        test.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
        test.setGravity(Gravity.CENTER);
        test.setPadding(dp(8), dp(14), dp(8), dp(4));
        ll.addView(test, new LinearLayout.LayoutParams(MATCH, WRAP));

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
        cancel.setOnClickListener(v -> setContentView(buildSettingsScreen())); // Store opens from Settings -> back to Settings

        LinearLayout.LayoutParams mlp = new LinearLayout.LayoutParams(MATCH, WRAP);
        mlp.topMargin = dp(12);
        ll.addView(msg, mlp);

        // Pre-fill the non-secret fields from the current config (GET-STORE), off the UI thread.
        new Thread(() -> {
            final java.util.HashMap<String, String> m = new java.util.HashMap<>();
            for (String ln : sendDaemonLines("GET-STORE\n")) {
                int i = ln.indexOf('=');
                if (i > 0) m.put(ln.substring(0, i), ln.substring(i + 1));
            }
            final boolean configured = "yes".equals(m.get("configured"));
            final String provider = providerFromEndpoint(m.get("endpoint"));
            runOnUiThread(() -> {
                if (m.get("endpoint") != null) endpointF.setText(m.get("endpoint"));
                if (m.get("bucket") != null) bucketF.setText(m.get("bucket"));
                String rg = m.get("region");
                if (rg != null && !rg.equals("auto")) regionF.setText(rg);
                if (configured) { // show a pending banner; the live PING-STORE below resolves it
                    conn.setText("checking " + provider + "…");
                    conn.setTextColor(MUTED);
                    conn.setVisibility(View.VISIBLE);
                }
                if ("yes".equals(m.get("keyset"))) {
                    msg.setTextColor(MUTED);
                    msg.setText("a key is already set — re-enter both keys to change it");
                } else {
                    msg.setTextColor(ACCENT);
                    msg.setText("no store yet — set one up to create or unlock a profile");
                }
            });
            // Live reachability (DIA-20260624-03): PING-STORE uses the daemon's saved creds (no secret needed
            // client-side), so the banner reflects whether the store is actually reachable, like Wi-Fi.
            if (configured) {
                final String r = sendDaemon("PING-STORE\n");
                runOnUiThread(() -> {
                    boolean ok = r.startsWith("OK");
                    conn.setText(ok ? "✓ Connected to " + provider
                            : provider + (r.startsWith("ERR-NET") ? " — can't reach it (offline?)"
                            : r.startsWith("ERR-AUTH") ? " — saved keys rejected"
                            : r.startsWith("ERR-BUCKET") ? " — bucket not found"
                            : " — unreachable"));
                    conn.setTextColor(ok ? ACCENT : MUTED);
                });
            }
        }).start();

        save.setOnClickListener(v -> {
            final String ep = endpointF.getText().toString().trim();
            final String bk = bucketF.getText().toString().trim();
            final String rg = regionF.getText().toString().trim();
            final String ak = keyF.getText().toString().trim();
            final String sk = secretF.getText().toString();
            if (ep.isEmpty() || bk.isEmpty() || ak.isEmpty() || sk.isEmpty()) {
                msg.setTextColor(MUTED);
                msg.setText("fill in URL, bucket, access key, and secret key");
                return;
            }
            save.setEnabled(false);
            msg.setTextColor(ACCENT);
            msg.setText("saving…");
            new Thread(() -> {
                String r = sendDaemon("SET-STORE\n" + ep + "\n" + rg + "\n" + bk + "\n" + ak + "\n" + sk + "\n");
                runOnUiThread(() -> {
                    if (r.startsWith("OK")) {
                        // #27: don't park "store saved" on the gate's SHARED result view -- it lingered onto the
                        // gate after Settings -> Store -> back. Clear any stale result + show a transient toast.
                        result.setText("");
                        setContentView(gateView);
                        android.widget.Toast.makeText(ChooserActivity.this, "Store saved",
                                android.widget.Toast.LENGTH_SHORT).show();
                    } else {
                        save.setEnabled(true);
                        msg.setTextColor(MUTED);
                        msg.setText("couldn't save the store — check the fields");
                    }
                });
            }).start();
        });

        test.setOnClickListener(v -> {
            final String ep = endpointF.getText().toString().trim();
            final String bk = bucketF.getText().toString().trim();
            final String rg = regionF.getText().toString().trim();
            final String ak = keyF.getText().toString().trim();
            final String sk = secretF.getText().toString();
            if (ep.isEmpty() || bk.isEmpty() || ak.isEmpty() || sk.isEmpty()) {
                msg.setTextColor(MUTED);
                msg.setText("fill in all fields to test");
                return;
            }
            test.setEnabled(false);
            msg.setTextColor(ACCENT);
            msg.setText("testing…");
            new Thread(() -> {
                String r = sendDaemon("TEST-STORE\n" + ep + "\n" + rg + "\n" + bk + "\n" + ak + "\n" + sk + "\n");
                final boolean ok = r.startsWith("OK");
                final String m2;
                if (ok) m2 = "connection OK — store reachable";
                else if (r.startsWith("ERR-AUTH")) m2 = "auth failed — check the access key + secret";
                else if (r.startsWith("ERR-BUCKET")) m2 = "reached the store, but that bucket wasn't found";
                else if (r.startsWith("ERR-NET")) m2 = "can't reach that URL — check it and your network";
                else m2 = "couldn't test — check the fields";
                runOnUiThread(() -> {
                    test.setEnabled(true);
                    msg.setTextColor(ok ? ACCENT : MUTED);
                    msg.setText(m2);
                });
            }).start();
        });

        android.widget.ScrollView sc = new android.widget.ScrollView(this);
        sc.setBackgroundColor(BG);
        sc.addView(ll);
        return sc;
    }

    /** In-gate Wi-Fi onboarding. A freshly flashed / factory-reset device loses its saved wifi with /data,
     *  and the kiosk blocks Settings -- so without this a wifi-only device can never reach its store to
     *  bootstrap. The chooser is the DEVICE OWNER, so it adds + enables a network via WifiManager (device
     *  owners are exempt from the API-29+ restriction that blocks normal apps from self-configuring wifi).
     *  In-app screen, NOT a launch into Settings, so the Lock-Task kiosk is never dropped (no escape). */
    private View buildWifiScreen() {
        LinearLayout ll = new LinearLayout(this);
        ll.setOrientation(LinearLayout.VERTICAL);
        ll.setGravity(Gravity.CENTER_HORIZONTAL);
        ll.setBackgroundColor(BG);
        ll.setPadding(dp(28), dp(52), dp(28), dp(36));

        TextView t = new TextView(this);
        t.setText("Wi-Fi");
        t.setTextColor(INK);
        t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 26);
        t.setGravity(Gravity.CENTER);
        ll.addView(t);

        TextView sub = new TextView(this);
        sub.setText("Join a network so this device can reach your store. A fresh device has no saved Wi-Fi.");
        sub.setTextColor(MUTED);
        sub.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        sub.setGravity(Gravity.CENTER);
        sub.setLineSpacing(dp(3), 1f);
        LinearLayout.LayoutParams slp = new LinearLayout.LayoutParams(MATCH, WRAP);
        slp.topMargin = dp(10);
        slp.bottomMargin = dp(24);
        ll.addView(sub, slp);

        // Nearby-networks picker: scan + list tappable SSIDs so the user needn't type. Best-effort -- if the
        // scan returns nothing (perm/sepolicy/hidden network), the manual fields below still work.
        final TextView netStatus = new TextView(this);
        netStatus.setText("scanning for networks…");
        netStatus.setTextColor(MUTED);
        netStatus.setTextSize(TypedValue.COMPLEX_UNIT_SP, 12);
        netStatus.setGravity(Gravity.CENTER);
        LinearLayout.LayoutParams nslp = new LinearLayout.LayoutParams(MATCH, WRAP);
        nslp.bottomMargin = dp(8);
        ll.addView(netStatus, nslp);

        final LinearLayout netList = new LinearLayout(this);
        netList.setOrientation(LinearLayout.VERTICAL);
        LinearLayout.LayoutParams nllp = new LinearLayout.LayoutParams(MATCH, WRAP);
        nllp.bottomMargin = dp(18);
        ll.addView(netList, nllp);

        final EditText ssidF = field("network name (SSID)", false);
        ll.addView(ssidF, fieldLp(dp(11)));
        final EditText pwF = field("Wi-Fi password (blank if open)", true);
        ll.addView(pwF, fieldLp(dp(20)));

        // "Forget this network" (DIA-20260624-04): the gate could JOIN Wi-Fi but had no way to disconnect --
        // the kiosk blocks Quick Settings / the shade. Declared here so the scan thread can reveal it when
        // connected; added to the layout below Connect. The gate is device owner, so removeNetwork is allowed.
        final TextView forget = new TextView(this);
        forget.setText("Forget this network");
        forget.setTextColor(ACCENT);
        forget.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
        forget.setGravity(Gravity.CENTER);
        forget.setPadding(dp(8), dp(12), dp(8), dp(4));
        forget.setVisibility(View.GONE);

        // Kick a scan off the UI thread, then populate the list. Tapping a row fills the SSID (and focuses
        // the password for a secured network). An empty result just leaves the manual field in place.
        new Thread(() -> {
            final java.util.List<String[]> nets = scanWifi();
            final String connected = connectedSsid(); // read while scanWifi() still holds the location grant
            runOnUiThread(() -> {
                if (connected != null) {
                    // Already online -- reassure instead of the "fresh device has no Wi-Fi" copy; the picker
                    // + fields stay below so the user can still switch networks.
                    sub.setText("✓ Connected to " + connected);
                    sub.setTextColor(ACCENT);
                    forget.setVisibility(View.VISIBLE); // offer disconnect only when actually connected
                }
                netStatus.setText(nets.isEmpty()
                        ? (connected != null ? "no other networks found"
                                             : "no networks found — type the name below")
                        : (connected != null ? "tap another network to switch, or type one below:"
                                             : "tap a network, or type one below:"));
                for (String[] n : nets) {
                    final String ssid = n[0];
                    final boolean secured = "1".equals(n[1]);
                    TextView row = makeNetRow(ssid, secured);
                    row.setOnClickListener(v -> {
                        ssidF.setText(ssid);
                        pwF.setText("");
                        if (secured) pwF.requestFocus();
                    });
                    LinearLayout.LayoutParams rlp = new LinearLayout.LayoutParams(MATCH, WRAP);
                    rlp.topMargin = dp(6);
                    netList.addView(row, rlp);
                }
            });
        }).start();

        final Button connect = accentButton("Connect");
        ll.addView(connect, new LinearLayout.LayoutParams(MATCH, WRAP));
        ll.addView(forget, new LinearLayout.LayoutParams(MATCH, WRAP));

        final TextView msg = new TextView(this);
        msg.setTextColor(MUTED);
        msg.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        msg.setGravity(Gravity.CENTER);
        LinearLayout.LayoutParams mlp = new LinearLayout.LayoutParams(MATCH, WRAP);
        mlp.topMargin = dp(14);
        ll.addView(msg, mlp);

        TextView back = new TextView(this);
        back.setText("Back");
        back.setTextColor(ACCENT);
        back.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
        back.setGravity(Gravity.CENTER);
        back.setPadding(dp(8), dp(18), dp(8), dp(8));
        ll.addView(back, new LinearLayout.LayoutParams(MATCH, WRAP));
        back.setOnClickListener(v -> { setContentView(buildSettingsScreen()); stopScanLocation(); }); // Wi-Fi opens from Settings -> back to Settings

        connect.setOnClickListener(v -> {
            final String ssid = ssidF.getText().toString().trim();
            final String pw = pwF.getText().toString();
            if (ssid.isEmpty()) {
                msg.setTextColor(MUTED);
                msg.setText("enter a network name");
                return;
            }
            connect.setEnabled(false);
            msg.setTextColor(ACCENT);
            msg.setText("connecting…");
            new Thread(() -> {
                final String r = connectWifi(ssid, pw);
                runOnUiThread(() -> {
                    connect.setEnabled(true);
                    if (r.startsWith("OK")) {
                        stopScanLocation();
                        // Land back on Settings (the screen Wi-Fi opened from) to keep configuring -- the store
                        // setup needs Wi-Fi -- instead of dropping to the gate (DIA-20260622: same routing as the
                        // Back/Cancel paths, DIA-20260618-18). A user who just wants to sign in taps Back to the gate.
                        android.widget.Toast.makeText(ChooserActivity.this, "Wi-Fi connected", android.widget.Toast.LENGTH_SHORT).show();
                        setContentView(buildSettingsScreen());
                    } else {
                        msg.setTextColor(MUTED);
                        msg.setText(r.startsWith("ERR ") ? r.substring(4) : r);
                    }
                });
            }).start();
        });

        forget.setOnClickListener(v -> {
            forget.setEnabled(false);
            msg.setTextColor(MUTED);
            msg.setText("disconnecting…");
            new Thread(() -> {
                final String r = forgetWifi();
                runOnUiThread(() -> {
                    forget.setEnabled(true);
                    if (r.startsWith("OK")) {
                        forget.setVisibility(View.GONE);
                        sub.setText("Join a network so this device can reach your store. A fresh device has no saved Wi-Fi.");
                        sub.setTextColor(MUTED);
                        msg.setTextColor(ACCENT);
                        msg.setText("disconnected — join a network below");
                    } else {
                        msg.setTextColor(MUTED);
                        msg.setText("couldn't forget the network");
                    }
                });
            }).start();
        });

        android.widget.ScrollView sc = new android.widget.ScrollView(this);
        sc.setBackgroundColor(BG);
        sc.addView(ll);
        return sc;
    }

    /** SSID of the currently-associated Wi-Fi network, or null if not connected / unknown. The SSID is
     *  location-gated: call this ONLY while the scan's location grant is held (i.e. from the scan thread) --
     *  otherwise getSSID() returns the redacted UNKNOWN_SSID ("<unknown ssid>"). getConnectionInfo() is
     *  deprecated but device-owner-allowed (same posture as connectWifi's WifiConfiguration path). */
    private String connectedSsid() {
        try {
            android.net.wifi.WifiManager wm = (android.net.wifi.WifiManager)
                    getApplicationContext().getSystemService(WIFI_SERVICE);
            if (wm == null || !wm.isWifiEnabled()) return null;
            android.net.wifi.WifiInfo info = wm.getConnectionInfo();
            if (info == null || info.getNetworkId() == -1) return null; // -1 => not associated
            String ssid = info.getSSID();
            if (ssid == null) return null;
            ssid = ssid.replaceAll("^\"|\"$", ""); // getSSID() wraps a UTF-8 SSID in quotes
            if (ssid.isEmpty()
                    || ssid.equals(android.net.wifi.WifiManager.UNKNOWN_SSID)
                    || ssid.equals("<unknown ssid>")) return null; // redacted (no location grant) or none
            return ssid;
        } catch (Exception e) {
            android.util.Log.w("NowhereChooser", "connectedSsid", e);
            return null;
        }
    }

    /** Disconnect + forget the current Wi-Fi network (the gate's only disconnect path -- the kiosk blocks
     *  Quick Settings). The gate is device owner, so WifiManager.removeNetwork works on the network it added;
     *  getNetworkId() is NOT location-gated (unlike getSSID), so this is safe to call without the scan grant. */
    private String forgetWifi() {
        try {
            android.net.wifi.WifiManager wm = (android.net.wifi.WifiManager)
                    getApplicationContext().getSystemService(WIFI_SERVICE);
            if (wm == null) return "ERR no wifi service";
            int netId = wm.getConnectionInfo().getNetworkId();
            if (netId == -1) return "ERR not connected";
            wm.removeNetwork(netId);
            wm.disconnect();
            wm.saveConfiguration();
            return "OK";
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "forgetWifi", t);
            return "ERR " + (t.getMessage() == null ? "failed" : t.getMessage());
        }
    }

    /** Scan for nearby Wi-Fi and return [ssid, secured("1"/"0")] strongest-first, deduped by SSID. Runs OFF
     *  the UI thread. The device owner self-grants the scan permission (NEARBY_WIFI_DEVICES, neverForLocation
     *  on T+ -> no location toggle needed). Best-effort: any failure returns an empty list and the screen
     *  falls back to the manual SSID field. WifiManager scan needs the wifi_service the gate domain is
     *  granted (sepolicy); getScanResults returns [] without the permission, which degrades gracefully. */
    private java.util.List<String[]> scanWifi() {
        java.util.ArrayList<String[]> out = new java.util.ArrayList<>();
        android.app.admin.DevicePolicyManager dpm =
                (android.app.admin.DevicePolicyManager) getSystemService(DEVICE_POLICY_SERVICE);
        android.content.ComponentName admin = new android.content.ComponentName(this, AdminReceiver.class);
        boolean owner = dpm != null && dpm.isDeviceOwnerApp(getPackageName());
        try {
            // Device owner self-grants the scan perms + enables location for the scan. The grants STAY (we
            // never runtime-revoke them -- revoking a held permission RESTARTS this persistent process and the
            // relaunch doesn't land cleanly on the gate). Privacy at rest comes from the location master
            // toggle, which stopScanLocation() turns back off when leaving the screen / at rest. Granting does
            // NOT restart, so granting here is safe. NOTE: NEARBY_WIFI_DEVICES alone is NOT enough on this
            // build -- getScanResults() silently returns empty unless the caller also holds ACCESS_FINE_LOCATION
            // (+ location on). So grant both.
            if (owner) {
                for (String perm : new String[]{"android.permission.NEARBY_WIFI_DEVICES",
                        android.Manifest.permission.ACCESS_FINE_LOCATION}) {
                    try {
                        dpm.setPermissionGrantState(admin, getPackageName(), perm,
                                android.app.admin.DevicePolicyManager.PERMISSION_GRANT_STATE_GRANTED);
                    } catch (Throwable ignore) {}
                }
                try { dpm.setLocationEnabled(admin, true); } catch (Throwable ignore) {}
            }
            Thread.sleep(600); // let the grants settle before scanning
            android.net.wifi.WifiManager wm =
                    (android.net.wifi.WifiManager) getApplicationContext().getSystemService(WIFI_SERVICE);
            if (wm == null) return out;
            if (!wm.isWifiEnabled()) {
                wm.setWifiEnabled(true);
                for (int i = 0; i < 12 && !wm.isWifiEnabled(); i++) Thread.sleep(300);
            }
            wm.startScan();
            java.util.List<android.net.wifi.ScanResult> results = null;
            for (int i = 0; i < 8; i++) { // give the scan a few seconds to land
                Thread.sleep(700);
                results = wm.getScanResults();
                if (results != null && !results.isEmpty()) break;
            }
            if (results == null) return out;
            java.util.Collections.sort(results, (a, b) -> Integer.compare(b.level, a.level)); // strongest first
            java.util.LinkedHashMap<String, String[]> best = new java.util.LinkedHashMap<>();
            for (android.net.wifi.ScanResult r : results) {
                String ssid = r.SSID;
                if (ssid == null || ssid.trim().isEmpty() || best.containsKey(ssid)) continue;
                boolean secured = r.capabilities != null && (r.capabilities.contains("WPA")
                        || r.capabilities.contains("WEP") || r.capabilities.contains("PSK")
                        || r.capabilities.contains("SAE"));
                best.put(ssid, new String[]{ssid, secured ? "1" : "0"});
            }
            out.addAll(best.values());
        } catch (Throwable e) {
            android.util.Log.w("NowhereChooser", "wifi scan", e);
        }
        return out;
    }

    /** Drop the location capability the Wi-Fi scan needed, WITHOUT restarting the gate. The privacy property
     *  -- a kiosk gate at rest can't obtain a location fix -- comes from the location MASTER toggle being off;
     *  flipping it does not kill the app. We deliberately do NOT runtime-revoke FINE_LOCATION / NEARBY here:
     *  revoking a runtime permission an app currently holds KILLS the process, and on this persistent (-800)
     *  gate the kill + relaunch does NOT land cleanly back on the gate -- it leaves a half-restored Wi-Fi
     *  screen with the kiosk nav exposed, or a blank window (the "Wi-Fi Back doesn't work" bug). So the scan
     *  grants stay granted-but-inert (no fix is available while the master toggle is off), and only the toggle
     *  flips. Called when leaving the Wi-Fi screen (Back / successful connect) and from onResume at rest. */
    private void stopScanLocation() {
        try {
            android.app.admin.DevicePolicyManager dpm =
                    (android.app.admin.DevicePolicyManager) getSystemService(DEVICE_POLICY_SERVICE);
            android.content.ComponentName admin = new android.content.ComponentName(this, AdminReceiver.class);
            if (dpm == null || !dpm.isDeviceOwnerApp(getPackageName())) return;
            try { dpm.setLocationEnabled(admin, false); } catch (Throwable ignore) {}
        } catch (Throwable ignore) {}
    }

    /** A tappable network row for the Wi-Fi picker (SSID + a lock glyph if secured). */
    private TextView makeNetRow(String ssid, boolean secured) {
        TextView t = new TextView(this);
        t.setText(secured ? ssid + "   🔒" : ssid);
        t.setTextColor(FIELD_TX);
        t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 15);
        t.setPadding(dp(14), dp(13), dp(14), dp(13));
        GradientDrawable bg = new GradientDrawable();
        bg.setColor(FIELD_BG);
        bg.setCornerRadius(dp(10));
        t.setBackground(bg);
        return t;
    }

    /** Add + enable a Wi-Fi network as the device owner, then wait for an internet-capable connection.
     *  Returns "OK" or "ERR <human message>". Uses the deprecated-but-DO-allowed WifiConfiguration path. */
    private String connectWifi(String ssid, String pw) {
        try {
            android.net.wifi.WifiManager wm = (android.net.wifi.WifiManager)
                    getApplicationContext().getSystemService(WIFI_SERVICE);
            if (wm == null) return "ERR no Wi-Fi service on this device";
            if (!wm.isWifiEnabled()) {
                wm.setWifiEnabled(true); // device owner may toggle wifi
                for (int i = 0; i < 12 && !wm.isWifiEnabled(); i++) {
                    try { Thread.sleep(300); } catch (InterruptedException ignore) {}
                }
            }
            android.net.wifi.WifiConfiguration cfg = new android.net.wifi.WifiConfiguration();
            cfg.SSID = "\"" + ssid + "\"";
            if (pw == null || pw.isEmpty()) {
                cfg.allowedKeyManagement.set(android.net.wifi.WifiConfiguration.KeyMgmt.NONE);
            } else {
                cfg.preSharedKey = "\"" + pw + "\""; // WPA/WPA2 PSK
            }
            int netId = wm.addNetwork(cfg);
            if (netId < 0) netId = existingNetId(wm, ssid); // already configured -> reuse it
            if (netId < 0) return "ERR couldn't add that network — try again";
            wm.disconnect();
            wm.enableNetwork(netId, true);
            wm.reconnect();
            android.net.ConnectivityManager cm =
                    (android.net.ConnectivityManager) getSystemService(CONNECTIVITY_SERVICE);
            boolean associated = false;
            for (int i = 0; i < 40; i++) { // up to ~20s -- the system's internet validation can lag association
                try { Thread.sleep(500); } catch (InterruptedException ignore) {}
                android.net.Network nw = cm.getActiveNetwork();
                if (nw != null) {
                    android.net.NetworkCapabilities nc = cm.getNetworkCapabilities(nw);
                    if (nc != null && nc.hasTransport(android.net.NetworkCapabilities.TRANSPORT_WIFI)) {
                        associated = true; // wifi is the active network -> good enough to try a login
                        if (nc.hasCapability(android.net.NetworkCapabilities.NET_CAPABILITY_VALIDATED)
                                || nc.hasCapability(android.net.NetworkCapabilities.NET_CAPABILITY_INTERNET)) {
                            return "OK";
                        }
                    }
                }
            }
            // Associated but not yet validated within the window -> still likely usable; let the user proceed
            // (the login/daemon is the real reachability test). Only a never-associated network is a hard error.
            if (associated) return "OK";
            return "ERR couldn't join that network — check the name and password";
        } catch (Throwable e) {
            android.util.Log.w("NowhereChooser", "wifi connect", e);
            return "ERR Wi-Fi connect failed (" + e.getClass().getSimpleName() + ")";
        }
    }

    /** netId of an already-configured network matching ssid (device owner can read its own configs). */
    private int existingNetId(android.net.wifi.WifiManager wm, String ssid) {
        try {
            java.util.List<android.net.wifi.WifiConfiguration> cfgs = wm.getConfiguredNetworks();
            if (cfgs != null) {
                String q = "\"" + ssid + "\"";
                for (android.net.wifi.WifiConfiguration c : cfgs) {
                    if (q.equals(c.SSID)) return c.networkId;
                }
            }
        } catch (Throwable ignore) {}
        return -1;
    }

    /** Gate hint when no store is configured -> point the user at Store settings (they can't create/unlock yet). */
    private void showStoreHint() {
        runOnUiThread(() -> {
            result.setTextColor(ACCENT);
            result.setText("No store set — tap Store settings below to set one up");
        });
    }

    /** Send ROAM-IN and read the streamed reply: "PROGRESS <phase> <done> <total>" lines drive the restore
     *  bar; the first non-PROGRESS line is the verdict ("OK <uid>" | "BLANK"). Falls back gracefully (no
     *  progress lines for a tiny/instant restore -> the verdict arrives directly). */
    private String roamInStreaming(String name, String pw, int uid) {
        LocalSocket s = DaemonSocket.connect(); // #77: bounded connect; the streamed read below has its own long cap
        if (s == null) { android.util.Log.w("NowhereChooser", "roamInStreaming: daemon did not accept (wedged?)"); return ""; }
        try {
            s.setSoTimeout(120000); // restore does S3 I/O; streamed progress keeps the read alive
            OutputStream os = s.getOutputStream();
            // 4th line (E.3b): se_secret from this device's StrongBox for a hardened identity ("" if no SE).
            os.write(("ROAM-IN\n" + name + "\n" + pw + "\n" + uid + "\n" + seSecretHex(name) + "\n").getBytes("UTF-8"));
            os.flush();
            BufferedReader br = new BufferedReader(new InputStreamReader(s.getInputStream(), "UTF-8"));
            lastSealTs = 0;
            String line, verdict = "";
            while ((line = br.readLine()) != null) {
                line = line.trim();
                if (line.startsWith("PROGRESS ")) {
                    onRestoreProgress(line.substring("PROGRESS ".length()));
                } else if (line.startsWith("LASTSEAL ")) {
                    try { lastSealTs = Long.parseLong(line.substring("LASTSEAL ".length()).trim()); } catch (Throwable ignore) {}
                } else if (!line.isEmpty()) {
                    verdict = line;
                    break;
                }
            }
            return verdict;
        } catch (Exception e) {
            android.util.Log.w("NowhereChooser", "roamInStreaming", e);
            return "";
        } finally {
            try { s.close(); } catch (Throwable ignore) {}
        }
    }

    /** Drive the bar from a "<phase> <done> <total>" progress line. */
    private void onRestoreProgress(String s) {
        sawRestoreProgress = true; // #70/#75: any progress means the creds were accepted + the restore started
        String[] p = s.split("\\s+");
        if (p.length < 3) return;
        final String phase = p[0];
        int d, t;
        try { d = Integer.parseInt(p[1]); t = Integer.parseInt(p[2]); }
        catch (NumberFormatException e) { return; }
        final int pct = t > 0 ? (int) (100L * d / t) : 0;
        runOnUiThread(() -> {
            progressBar.setVisibility(View.VISIBLE);
            progressBar.setProgress(pct);
            result.setTextColor(ACCENT);
            result.setText(phaseLabel(phase) + " — " + pct + "%");
        });
    }

    private String phaseLabel(String phase) {
        switch (phase) {
            case "apps":   return "Restoring app data";
            case "secure": return "Restoring secure data";
            case "media":  return "Restoring photos & files";
            default:       return "Restoring your profile";
        }
    }

    /** True if there's an active network claiming internet -- used to skip a doomed restore (and a cryptic
     *  blank) when wifi isn't up yet. NET_CAPABILITY_INTERNET (not VALIDATED) so a just-connected link counts.
     *  Fail-open: if we can't tell, return true and let the login attempt proceed. */
    private boolean hasInternet() {
        try {
            android.net.ConnectivityManager cm =
                    (android.net.ConnectivityManager) getSystemService(CONNECTIVITY_SERVICE);
            if (cm == null) return true;
            android.net.Network nw = cm.getActiveNetwork();
            if (nw == null) return false;
            android.net.NetworkCapabilities nc = cm.getNetworkCapabilities(nw);
            return nc != null && nc.hasCapability(android.net.NetworkCapabilities.NET_CAPABILITY_INTERNET);
        } catch (Throwable t) {
            return true;
        }
    }

    /** A unix-seconds ts -> a short relative string ("just now" / "5 min ago" / "3 hours ago" / "2 days ago" / "MMM d"). */
    private static String relTime(long unixSec) {
        long d = System.currentTimeMillis() / 1000L - unixSec;
        if (d < 0) d = 0;
        if (d < 90) return "just now";
        if (d < 3600) return (d / 60) + " min ago";
        if (d < 86400) { long h = d / 3600; return h + (h == 1 ? " hour ago" : " hours ago"); }
        if (d < 7 * 86400) { long dd = d / 86400; return dd + (dd == 1 ? " day ago" : " days ago"); }
        return new java.text.SimpleDateFormat("MMM d", java.util.Locale.getDefault()).format(new java.util.Date(unixSec * 1000L));
    }

    private void showBlank() {
        runOnUiThread(() -> {
            result.setTextColor(MUTED);
            // Friendly but UNIFORM: a wrong passphrase and an unknown profile must read identically, or we'd
            // leak whether a name exists (and break duress/hidden-profile deniability). So one message covers
            // every failure -- wrong creds, unknown profile, or an unreachable store -- the same way.
            result.setText("Couldn't sign in — check your name, passphrase, and Wi-Fi");
            passField.setText("");
            if (confirmField != null) confirmField.setText("");
        });
    }

    private void showMsg(final String m) {
        runOnUiThread(() -> { result.setTextColor(MUTED); result.setText(m); });
    }

    /** Turn a "RATELIMIT <seconds>" daemon reply into a friendly wait message for the enrollment throttle. */
    private String rateLimitMsg(String reply) {
        int secs = 0;
        try { secs = Integer.parseInt(reply.trim().split("\\s+")[1]); } catch (Exception ignore) {}
        if (secs >= 90) return "too many new profiles — try again in about " + ((secs + 59) / 60) + " min";
        if (secs > 0)  return "too many new profiles — try again in " + secs + "s";
        return "too many new profiles — try again later";
    }

    /** Send one line-framed request to the root daemon over the AF_UNIX socket; return its reply line. */
    private String sendDaemon(String msg) {
        return DaemonSocket.roundTrip(msg, 90000); // #77 bounded connect; roam restore/seal does S3 I/O -> long read cap
    }

    private static volatile boolean reapWatcherStarted = false;
    private static volatile boolean signingOut = false;          // a no-reboot logoff's reap is still finishing
    private static volatile boolean sessionLive = false;         // a roamed session is logged in -> poll fast for a quick logoff (DIA-20260618-07)
    private static volatile boolean loginInProgress = false;     // a login is creating/roaming a user -> the gate orphan-sweep must not touch it (#73)
    private static volatile ChooserActivity sLiveGate = null;     // the foreground gate, for the reap watcher to update

    /** L4: read by IdleLogoffReceiver before auto-logging-off, to confirm a session is still live. */
    static boolean isSessionLive() { return sessionLive; }

    /** L4 idle-timeout backstop: while a session is live, arm an alarm on screen-off and cancel it on unlock;
     *  if it fires (locked + idle for IDLE_LOGOFF_MS) IdleLogoffReceiver auto-logs-off. The keyguard alone
     *  leaves CE warm (lock-model.md L3), so this timeout is the real at-rest bound. Replaced on each login. */
    private static void armIdleWatcher(final android.content.Context appCtx, final int uid) {
        synchronized (ChooserActivity.class) {
            cancelIdleWatcher(appCtx);
            cancelWipeWatcher(appCtx); // #3: a session is live again -> cancel any pending cold-lock hard-wipe
            if (sIdleTimeoutMs <= 0) return; // L5: the profile chose "Never" -> no idle auto-logoff
            // Backstop the screen-off-armed alarm below: force the OS to lock the keyguard after the same idle
            // window even if an app holds the screen AWAKE (a downloading map, video, navigation). Without this,
            // a keep-screen-on app means the screen never turns off -> the screen-off alarm never arms -> the
            // session never cold-locks or wipes, staying unlocked + decrypted indefinitely (DIA-20260630-39).
            // setMaximumTimeToLock overrides wake locks for the lock timer; the forced screen-off then feeds the
            // existing screen-off -> cold-lock -> 12 h-wipe chain.
            setMaxTimeToLock(appCtx, sIdleTimeoutMs);
            final android.app.AlarmManager am =
                    (android.app.AlarmManager) appCtx.getSystemService(android.content.Context.ALARM_SERVICE);
            sIdleAlarmPi = android.app.PendingIntent.getBroadcast(appCtx, 0,
                    new android.content.Intent(appCtx, IdleLogoffReceiver.class).putExtra("uid", uid),
                    android.app.PendingIntent.FLAG_UPDATE_CURRENT | android.app.PendingIntent.FLAG_IMMUTABLE);
            sIdleScreenOff = new android.content.BroadcastReceiver() {
                @Override public void onReceive(android.content.Context c, android.content.Intent i) {
                    // #29: arm only on the FIRST screen-off after the last unlock. Re-arming on every screen-off
                    // let a glance/notification (screen on->off without an unlock) restart the countdown, so the
                    // backstop could never elapse. The timer now counts continuously from the lock.
                    if (!sessionLive || sIdleAlarmPi == null || sIdleArmed) return;
                    long when = android.os.SystemClock.elapsedRealtime() + sIdleTimeoutMs;
                    try { am.setExactAndAllowWhileIdle(android.app.AlarmManager.ELAPSED_REALTIME_WAKEUP, when, sIdleAlarmPi); }
                    catch (Throwable t) { am.setAndAllowWhileIdle(android.app.AlarmManager.ELAPSED_REALTIME_WAKEUP, when, sIdleAlarmPi); }
                    sIdleArmed = true;
                }
            };
            sIdleUserPresent = new android.content.BroadcastReceiver() {
                @Override public void onReceive(android.content.Context c, android.content.Intent i) {
                    // #29: an actual unlock = active use -> cancel + disarm so the NEXT screen-off restarts the
                    // full idle countdown from that lock.
                    if (sIdleAlarmPi != null) try { am.cancel(sIdleAlarmPi); } catch (Throwable ignore) {}
                    sIdleArmed = false;
                }
            };
            try {
                appCtx.registerReceiver(sIdleScreenOff, new android.content.IntentFilter(android.content.Intent.ACTION_SCREEN_OFF));
                appCtx.registerReceiver(sIdleUserPresent, new android.content.IntentFilter(android.content.Intent.ACTION_USER_PRESENT));
            } catch (Throwable t) { android.util.Log.w("NowhereChooser", "arm idle watcher", t); }
        }
    }

    /** Tear down the idle watcher + its pending alarm (session ended, or replaced on a new login). */
    private static void cancelIdleWatcher(final android.content.Context appCtx) {
        synchronized (ChooserActivity.class) {
            sIdleArmed = false; // #29: a fresh login / teardown starts disarmed -> the first screen-off arms it
            setMaxTimeToLock(appCtx, 0); // drop the OS force-lock restriction (re-set by armIdleWatcher for a live session)
            if (sIdleAlarmPi != null) {
                try { ((android.app.AlarmManager) appCtx.getSystemService(android.content.Context.ALARM_SERVICE)).cancel(sIdleAlarmPi); } catch (Throwable ignore) {}
                sIdleAlarmPi = null;
            }
            if (sIdleScreenOff != null) { try { appCtx.unregisterReceiver(sIdleScreenOff); } catch (Throwable ignore) {} sIdleScreenOff = null; }
            if (sIdleUserPresent != null) { try { appCtx.unregisterReceiver(sIdleUserPresent); } catch (Throwable ignore) {} sIdleUserPresent = null; }
        }
    }

    /** L4 backstop (DIA-20260630-39): force the OS to lock after `ms` of user inactivity even when an app holds
     *  the screen awake (setMaximumTimeToLock overrides wake locks for the lock timer). ms=0 clears it. Device-owner
     *  only; wrapped so a policy hiccup never crashes the gate. */
    private static void setMaxTimeToLock(final android.content.Context appCtx, final long ms) {
        try {
            android.app.admin.DevicePolicyManager dpm =
                    (android.app.admin.DevicePolicyManager) appCtx.getSystemService(android.content.Context.DEVICE_POLICY_SERVICE);
            if (dpm == null || !dpm.isDeviceOwnerApp(appCtx.getPackageName())) return;
            dpm.setMaximumTimeToLock(new android.content.ComponentName(appCtx, AdminReceiver.class), ms);
        } catch (Throwable t) { android.util.Log.w("NowhereChooser", "setMaxTimeToLock", t); }
    }

    /** #3 hard-wipe backstop (DIA-20260626-03): arm the (default 12 h) alarm at COLD-LOCK. If it fires -- the
     *  session was never resumed -- ColdWipeReceiver removes the cold-locked user: the amnesiac escalation of an
     *  idle cold-lock, identical to the power-off boot-wipe but on a timer. Counts from cold-lock; cancelled when
     *  a session goes live again (armIdleWatcher) or on resume. `wipe=0` -> Never (lingers until a power-off). */
    private static void armWipeWatcher(final android.content.Context appCtx, final int uid) {
        synchronized (ChooserActivity.class) {
            cancelWipeWatcher(appCtx);
            if (sWipeTimeoutMs <= 0) return; // "Never" -> a cold-locked session lingers until a power-off boot-wipe
            final android.app.AlarmManager am =
                    (android.app.AlarmManager) appCtx.getSystemService(android.content.Context.ALARM_SERVICE);
            sWipeAlarmPi = android.app.PendingIntent.getBroadcast(appCtx, 1,
                    new android.content.Intent(appCtx, ColdWipeReceiver.class).putExtra("uid", uid),
                    android.app.PendingIntent.FLAG_UPDATE_CURRENT | android.app.PendingIntent.FLAG_IMMUTABLE);
            long when = android.os.SystemClock.elapsedRealtime() + sWipeTimeoutMs;
            // Exact + allow-while-idle: the wipe is an at-rest bound, so Doze must NOT defer it indefinitely.
            try { am.setExactAndAllowWhileIdle(android.app.AlarmManager.ELAPSED_REALTIME_WAKEUP, when, sWipeAlarmPi); }
            catch (Throwable t) { am.setAndAllowWhileIdle(android.app.AlarmManager.ELAPSED_REALTIME_WAKEUP, when, sWipeAlarmPi); }
            android.util.Log.i("NowhereChooser", "hard-wipe armed: user " + uid + " in " + (sWipeTimeoutMs / 60000) + " min");
        }
    }

    /** Cancel the pending hard-wipe alarm (the session resumed, or a new session went live). */
    private static void cancelWipeWatcher(final android.content.Context appCtx) {
        synchronized (ChooserActivity.class) {
            if (sWipeAlarmPi != null) {
                try { ((android.app.AlarmManager) appCtx.getSystemService(android.content.Context.ALARM_SERVICE)).cancel(sWipeAlarmPi); } catch (Throwable ignore) {}
                sWipeAlarmPi = null;
            }
        }
    }

    /** #3 hard-wipe: ColdWipeReceiver fired -> a cold-locked session was never resumed within the wipe window,
     *  so REMOVE it (amnesiac escalation -- the same removal a power-off boot-wipe does, on a timer). Guarded by
     *  the daemon's .coldlock marker: if the session was resumed (marker gone) or the marker is a different uid,
     *  do nothing. Off the main thread (socket round-trip + removeUser). (DIA-20260626-03) */
    static void coldWipe(final android.content.Context ctx, final int uid) {
        final android.content.Context app = ctx.getApplicationContext();
        new Thread(() -> {
            try {
                String cl = pollDaemon("GET-COLDLOCK\n");
                if (!cl.startsWith("COLDLOCK ")) {
                    android.util.Log.i("NowhereChooser", "hard-wipe: no cold-lock (resumed/gone) -> skip");
                    return;
                }
                String rest = cl.substring(9).trim(); // "<uid> <name>"
                int sp = rest.indexOf(' ');
                String clUid = sp > 0 ? rest.substring(0, sp) : rest;
                if (!clUid.equals(String.valueOf(uid))) {
                    android.util.Log.w("NowhereChooser", "hard-wipe: marker uid " + clUid + " != alarm uid " + uid + " -> skip");
                    return;
                }
                // Route the wipe through the PROVEN switch-to-gate logoff path. The wipe fires on user 0 (the
                // cold-lock already switched here), so a direct removeUser leaves the STOPPED gate black -- there's
                // no user switch to force a redraw. So briefly foreground the (still CE-LOCKED) roamed user, then run
                // the normal switch-back-to-gate reap: that switch to user 0 is what redraws the gate. The user
                // never unlocks -- we just pass THROUGH it to manufacture the switch. Screen is off throughout, so
                // its keyguard never shows; doRemove wakes the gate at the end. (DIA-20260626-03)
                pollDaemon("CLEAR-COLDLOCK\n"); // drop the marker -> the post-wipe gate is the fresh login, not "Welcome back"
                android.util.Log.i("NowhereChooser", "hard-wipe: switch-through user " + uid + " then logoff to gate");
                android.app.ActivityManager am =
                        (android.app.ActivityManager) app.getSystemService(Context.ACTIVITY_SERVICE);
                try { am.switchUser(android.os.UserHandle.of(uid)); } // onto the cold-locked user (its keyguard, screen off)
                catch (Throwable t) { android.util.Log.w("NowhereChooser", "coldWipe switch-in", t); }
                try { Thread.sleep(2500); } catch (InterruptedException ie) { Thread.currentThread().interrupt(); }
                doSwitch(app, uid);  // pre-warm the gate + switchUser(0) -> the redraw transition (logoff phase 1)
                try { Thread.sleep(2500); } catch (InterruptedException ie) { Thread.currentThread().interrupt(); }
                doRemove(app, uid);  // remove the roamed user (+ clean CE) -> the proven logoff phase 2
            } catch (Throwable t) {
                android.util.Log.w("NowhereChooser", "coldWipe", t);
            }
        }).start();
    }

    private static volatile boolean notifWatcherStarted = false;
    private static final String NOTIF_CHANNEL = "nowhere.backup";
    private static final int NOTIF_ID = 0x0D1A;
    // Device-storage warning (#82): the roamed profile's local copy lives in /data, so a big profile can fill
    // the phone (e.g. a 250GB store profile on a 64GB device) and break a restore/save. Warn before that.
    // Separate channel + id from the backup notice so the two are independent. Hysteresis (warn>=80, clear<77)
    // stops it flapping across the boundary on the 30s poll.
    private static final String NOTIF_STORAGE_CHANNEL = "nowhere.storage";
    private static final int NOTIF_STORAGE_ID = 0x0D1B;
    private static final int STORAGE_WARN_PCT = 80;
    private static final int STORAGE_CLEAR_PCT = 77;

    /**
     * Backup-health watcher (DIA-20260619-12), device-owner half. The agent seals the live session every
     * cycle and reports STALE over GET-SYNC-STATUS when a seal fails (offline / store unreachable). This
     * user-0 process polls that on a slow cadence while a session is live and posts/cancels a "Not backed
     * up" status-bar notification IN the foreground roamed user (createContextAsUser) -- the user-0 shade
     * isn't what they see. Runs in the same android:persistent process as the reap watcher, so it's never
     * frozen. The notification auto-clears the moment a seal succeeds again.
     */
    private void startSyncStatusWatcher() {
        if (android.os.UserHandle.myUserId() != 0) return; // posts cross-user; runs only on user 0
        if (notifWatcherStarted) return;
        notifWatcherStarted = true;
        final Context app = getApplicationContext();
        Thread t = new Thread(() -> {
            final android.app.admin.DevicePolicyManager dpm =
                    (android.app.admin.DevicePolicyManager) app.getSystemService(Context.DEVICE_POLICY_SERVICE);
            final android.content.ComponentName admin = new android.content.ComponentName(app, AdminReceiver.class);
            int shownForUid = -1;     // backup notice: the user we last posted to (-1 = nothing shown)
            int storShownForUid = -1; // storage notice (#82), tracked independently
            while (true) {
                try { Thread.sleep(30000); } catch (InterruptedException ie) { return; }
                int uid = sessionLive ? firstSecondaryUid(dpm, admin) : -1;
                if (uid < 0) { // no live roamed session -> clear any leftover notices
                    if (shownForUid >= 0) { cancelBackupNotice(app, shownForUid); shownForUid = -1; }
                    if (storShownForUid >= 0) { cancelStorageNotice(app, storShownForUid); storShownForUid = -1; }
                    continue;
                }
                boolean stale = pollDaemon("GET-SYNC-STATUS\n").startsWith("STALE");
                if (stale) {
                    postBackupNotice(app, uid);
                    shownForUid = uid;
                } else if (shownForUid >= 0) {
                    cancelBackupNotice(app, shownForUid);
                    shownForUid = -1;
                }
                // Device-storage warning (#82): local statfs of /data (no daemon, no network). Warn at >=80%
                // full, clear below 77% (hysteresis). This is DEVICE storage, distinct from the store quota.
                int pct = deviceUsedPct(app);
                if (pct >= STORAGE_WARN_PCT) {
                    postStorageNotice(app, uid, pct);
                    storShownForUid = uid;
                } else if (storShownForUid >= 0 && pct >= 0 && pct < STORAGE_CLEAR_PCT) {
                    cancelStorageNotice(app, storShownForUid);
                    storShownForUid = -1;
                }
            }
        }, "nowhere-backup-watcher");
        t.setDaemon(true);
        t.start();
    }

    /** Post the ongoing "not backed up" notification into roamed user <uid>'s shade. */
    private static void postBackupNotice(Context app, int uid) {
        try {
            Context uc = app.createContextAsUser(android.os.UserHandle.of(uid), 0);
            android.app.NotificationManager nm = uc.getSystemService(android.app.NotificationManager.class);
            if (nm == null) return;
            android.app.NotificationChannel ch = new android.app.NotificationChannel(
                    NOTIF_CHANNEL, "Backup status", android.app.NotificationManager.IMPORTANCE_LOW);
            ch.setDescription("Warns when your changes can't be backed up to your store");
            nm.createNotificationChannel(ch);
            android.app.Notification n = new android.app.Notification.Builder(uc, NOTIF_CHANNEL)
                    .setSmallIcon(R.drawable.ic_backup_warn)
                    .setContentTitle("Not backed up")
                    .setContentText("Recent changes aren't being saved to your store. Check your connection.")
                    .setStyle(new android.app.Notification.BigTextStyle().bigText(
                            "Recent changes aren't being saved to your store. They'll sync once you're back online."))
                    .setOngoing(true)
                    .setShowWhen(false)
                    .build();
            nm.notify(NOTIF_ID, n);
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "postBackupNotice", t);
        }
    }

    /** Clear the "not backed up" notification from roamed user <uid>'s shade. */
    private static void cancelBackupNotice(Context app, int uid) {
        try {
            Context uc = app.createContextAsUser(android.os.UserHandle.of(uid), 0);
            android.app.NotificationManager nm = uc.getSystemService(android.app.NotificationManager.class);
            if (nm != null) nm.cancel(NOTIF_ID);
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "cancelBackupNotice", t);
        }
    }

    /** Percent of the /data filesystem in use (0-100), or -1 on error. StatFs on our own data dir measures the
     *  /data partition, which is shared across users on this device -- so it's the device-wide fill. Local +
     *  cheap; no daemon, no network. (#82) */
    private static int deviceUsedPct(Context app) {
        try {
            android.os.StatFs sf = new android.os.StatFs(app.getFilesDir().getPath());
            long total = sf.getTotalBytes();
            if (total <= 0) return -1;
            long used = total - sf.getAvailableBytes();
            long pct = used * 100 / total;
            return pct < 0 ? 0 : (pct > 100 ? 100 : (int) pct);
        } catch (Throwable t) {
            return -1;
        }
    }

    /** Post the ongoing "Storage almost full" notification into roamed user <uid>'s shade. (#82) */
    private static void postStorageNotice(Context app, int uid, int pct) {
        try {
            Context uc = app.createContextAsUser(android.os.UserHandle.of(uid), 0);
            android.app.NotificationManager nm = uc.getSystemService(android.app.NotificationManager.class);
            if (nm == null) return;
            android.app.NotificationChannel ch = new android.app.NotificationChannel(
                    NOTIF_STORAGE_CHANNEL, "Storage", android.app.NotificationManager.IMPORTANCE_LOW);
            ch.setDescription("Warns when this phone's storage is almost full");
            nm.createNotificationChannel(ch);
            android.app.Notification n = new android.app.Notification.Builder(uc, NOTIF_STORAGE_CHANNEL)
                    .setSmallIcon(R.drawable.ic_backup_warn)
                    .setContentTitle("Storage almost full")
                    .setContentText("This phone is " + pct + "% full.")
                    .setStyle(new android.app.Notification.BigTextStyle().bigText(
                            "This phone is " + pct + "% full. Free up space, or new downloads and files may not save."))
                    .setOngoing(true)
                    .setShowWhen(false)
                    .build();
            nm.notify(NOTIF_STORAGE_ID, n);
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "postStorageNotice", t);
        }
    }

    /** Clear the "Storage almost full" notification from roamed user <uid>'s shade. (#82) */
    private static void cancelStorageNotice(Context app, int uid) {
        try {
            Context uc = app.createContextAsUser(android.os.UserHandle.of(uid), 0);
            android.app.NotificationManager nm = uc.getSystemService(android.app.NotificationManager.class);
            if (nm != null) nm.cancel(NOTIF_STORAGE_ID);
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "cancelStorageNotice", t);
        }
    }

    /** The current ephemeral roamed user id (the single secondary user), or -1 if none. */
    private static int firstSecondaryUid(android.app.admin.DevicePolicyManager dpm,
                                         android.content.ComponentName admin) {
        try {
            for (android.os.UserHandle uh : dpm.getSecondaryUsers(admin)) return uh.getIdentifier();
        } catch (Throwable t) { /* fall through */ }
        return -1;
    }

    /**
     * Reboot-free logoff, device-owner half. ProfileActivity (on the roamed secondary user) tells the daemon
     * LOGOUT; the daemon seals + queues a "reap". But neither the secondary-user UI nor the su:s0 worker can
     * switch users / stop a user (device-owner wall), and the daemon can't push to us (its accept loop is
     * serial). So THIS user-0 process -- the device owner -- polls the daemon and, when a reap is pending,
     * switches back to the gate and stops+removes the ephemeral user (which wipes its /data). Started once
     * per process, user 0 only; a daemon thread that survives the gate Activity finishing. If this process
     * is dead, the daemon's own 8s fallback reboots instead.
     */
    private void startReapWatcher() {
        if (android.os.UserHandle.myUserId() != 0) return; // device-owner teardown runs only on user 0
        if (reapWatcherStarted) return;
        reapWatcherStarted = true;
        final Context app = getApplicationContext();
        Thread t = new Thread(() -> {
            android.os.Handler main = new android.os.Handler(android.os.Looper.getMainLooper());
            final android.os.PowerManager pm =
                    (android.os.PowerManager) app.getSystemService(Context.POWER_SERVICE);
            final android.app.admin.DevicePolicyManager dpm =
                    (android.app.admin.DevicePolicyManager) app.getSystemService(Context.DEVICE_POLICY_SERVICE);
            final android.content.ComponentName admin = new android.content.ComponentName(app, AdminReceiver.class);
            while (true) {
                String reply = pollDaemon("POLL-REAP\n");
                if (reply.startsWith("SWITCH ")) { // phase 1: switch to the gate FAST (before the seal)
                    try {
                        final int uid = Integer.parseInt(reply.substring(7).trim());
                        // screenOn is observed FALSE here even with the display on -- PowerManager.isInteractive()
                        // reads false from this backgrounded user-0 process, which is why the poll keys off
                        // sessionLive rather than the screen state (DIA-20260618-07).
                        android.util.Log.i("NowhereChooser", "reap watcher: SWITCH (screenOn="
                                + (pm != null && pm.isInteractive()) + ")");
                        main.post(() -> doSwitch(app, uid));
                    } catch (NumberFormatException ignore) {
                    }
                } else if (reply.startsWith("REAP ")) { // phase 2a: remove the user (the daemon has sealed it)
                    try {
                        final int uid = Integer.parseInt(reply.substring(5).trim());
                        main.post(() -> doRemove(app, uid));
                    } catch (NumberFormatException ignore) {
                    }
                } else if (reply.startsWith("LOCK ")) { // phase 2b: P3 cold-lock -- STOP (not remove) -> FBE-lock
                    try {
                        final int uid = Integer.parseInt(reply.substring(5).trim());
                        main.post(() -> doColdLock(app, uid));
                    } catch (NumberFormatException ignore) {
                    }
                }
                // Adaptive cadence to shrink the logoff escape window (DIA-20260618-07). The chooser is
                // android:persistent so this process is never frozen and the watcher genuinely runs at this rate
                // (without persistent it was cached-empty + frozen and lagged ~1.5s regardless of nap). Poll
                // FAST (~250ms) whenever a logoff could be in flight -- i.e. a roamed session is live, or a reap
                // is mid-flight -- so the SWITCH yanks back to the gate almost immediately (no window for a
                // gesture-nav recents swipe). `sessionLive` is set at login-handoff + cleared after the reap;
                // it does NOT depend on PowerManager.isInteractive(), which reads false from this backgrounded
                // user-0 process even with the screen on (that bug kept the old screen-gated poll at ~1.5s).
                // Parked at the gate (no secondary user) there's nothing to reap, so idle right down (15s) to
                // keep the resident process cheap; that slow tier also self-heals sessionLive if a process
                // restart reset it mid-session.
                long nap;
                if (signingOut || sessionLive) {
                    nap = 250;
                } else {
                    try { if (!dpm.getSecondaryUsers(admin).isEmpty()) sessionLive = true; }
                    catch (Throwable t2) { /* unsure -> fall through to the idle tier */ }
                    nap = sessionLive ? 250 : 15000;
                }
                try { Thread.sleep(nap); } catch (InterruptedException ie) { return; }
            }
        }, "nowhere-reap-watcher");
        t.setDaemon(true);
        t.start();
    }

    /** One short request/reply to the daemon (used by the reap watcher's poll + coldWipe's re-check). #77:
     *  bounded connect via DaemonSocket -- a wedged daemon here would otherwise stall the reap poll forever (a
     *  queued logoff never reaps) or block the 12h hard-wipe; now it just returns "" and the caller retries. */
    private static String pollDaemon(String msg) {
        return DaemonSocket.roundTrip(msg, 5000);
    }

    /** No-reboot logoff PHASE 1 (device owner, user 0): pre-warm the gate (cuts the launcher flash) and switch
     *  to user 0, backgrounding the roamed ephemeral user -- but do NOT remove it yet (the daemon is still
     *  sealing its data). The reap watcher polls fast while the screen is on and the chooser process is kept
     *  unfrozen (android:persistent), so this runs within ~250ms of logoff (before the S3 seal) -- the gate
     *  reclaims the foreground almost at once, leaving essentially no window to gesture out of "Logging off…". */
    private static void doSwitch(Context ctx, final int uid) {
        try {
            signingOut = true; // the gate's onResume will show "Signing out…" until phase 2 (remove) finishes
            ctx.startActivity(new Intent(ctx, ChooserActivity.class)
                    .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK | Intent.FLAG_ACTIVITY_SINGLE_TOP));
            android.app.ActivityManager am =
                    (android.app.ActivityManager) ctx.getSystemService(Context.ACTIVITY_SERVICE);
            am.switchUser(android.os.UserHandle.of(0));
            android.util.Log.i("NowhereChooser", "no-reboot logoff: switched to gate; user " + uid + " backgrounded, awaiting seal");
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "doSwitch", t);
        }
    }

    /** No-reboot logoff PHASE 2 (device owner, user 0): the daemon has SEALED the (backgrounded) user, so
     *  REMOVE it now -- deletes its /data (FBE key destruction == the reboot-wipe) -- then VERIFY the wipe and
     *  reboot if it somehow didn't happen, so a logged-off profile can never linger on device. The switch in
     *  phase 1 has already settled (a poll apart), so removeUser fires without an extra defer. */
    private static void doRemove(Context ctx, final int uid) {
        try {
            final Context app = ctx.getApplicationContext();
            final android.os.Handler h = new android.os.Handler(android.os.Looper.getMainLooper());
            final android.app.admin.DevicePolicyManager dpm =
                    (android.app.admin.DevicePolicyManager) app.getSystemService(Context.DEVICE_POLICY_SERVICE);
            final android.content.ComponentName admin = new android.content.ComponentName(app, AdminReceiver.class);
            boolean ok;
            try { ok = dpm.removeUser(admin, android.os.UserHandle.of(uid)); }
            catch (Throwable t) { ok = false; android.util.Log.w("NowhereChooser", "removeUser", t); }
            android.util.Log.i("NowhereChooser", "no-reboot logoff: removeUser " + uid + " ok=" + ok);
            // removeUser usually returns false yet schedules+completes the removal, so verify the REAL state
            // (the user record -- deleted together with its /data) by polling, and reboot only if it is still
            // there after a grace window. Closes the hole where a logged-off profile's data could survive a
            // failed in-place wipe.
            h.postDelayed(new Runnable() {
                int tries = 0;
                @Override public void run() {
                    if (!userPresent(dpm, admin, uid)) {
                        android.util.Log.i("NowhereChooser", "no-reboot logoff: user " + uid + " wiped");
                        cleanUserStorage(uid); // removeUser leaves /data/system_ce/<uid> -> a reused uid would crash; rm it
                        signingOut = false; // reap done -> drop the "Signing out…" notice, re-enable the gate
                        // Clear the gate-side "Saving your data… N%" readout + bar now the seal + reap are done.
                        try {
                            ChooserActivity g = sLiveGate;
                            if (g != null) {
                                if (g.result != null) { String rt = String.valueOf(g.result.getText()); if (rt.startsWith("Saving your data") || rt.startsWith("Preparing your data")) g.result.setText(""); }
                                if (g.progressBar != null) g.progressBar.setVisibility(View.GONE);
                                g.applySigningOutUi();
                            }
                        } catch (Throwable ignore) {}
                        sessionLive = false; // no session left -> the reap watcher can idle down (DIA-20260618-07)
                        // L2 (DIA-20260623-14): if the session logged off before its keyguard was ever armed
                        // (no screen-off yet), the one-shot ACTION_SCREEN_OFF armer is still registered -> cancel
                        // it so it can't fire on the gate (or against a reaped uid) after the session is gone.
                        synchronized (ChooserActivity.class) {
                            if (sCredArmer != null) { try { app.unregisterReceiver(sCredArmer); } catch (Throwable ignore) {} sCredArmer = null; }
                        }
                        cancelIdleWatcher(app); // L4: tear down the idle-logoff alarm + screen watchers
                        // L4 display polish (DIA-20260623-20): an idle/failed auto-logoff reaps the user while
                        // the screen is OFF, so the switch to the gate didn't repaint -- on wake the display
                        // kept a stale roamed keyguard frame. Turn the screen on FROM THE GATE ACTIVITY (a
                        // valid context + window; the app context's getSystemService(POWER) returned null in
                        // this reap callback): a FLAG_TURN_SCREEN_ON flag + an ACQUIRE_CAUSES_WAKEUP wakelock
                        // render the gate. No-op for a user-initiated logoff (screen already on).
                        ChooserActivity g = sLiveGate;
                        if (g != null) g.runOnUiThread(() -> {
                            g.applySigningOutUi();
                            // Re-bind the IME now the reap is DONE (signingOut=false). The onResume nudge fired
                            // mid-reap (signingOut still true) and bailed, leaving the gate keyboard-less; fire it
                            // here so the post-logoff gate gets its keyboard back. (keyboard-at-gate, DIA-20260624-11)
                            // If this reap completed screen-OFF, the immediate nudge can't render -- the
                            // ACTION_SCREEN_ON handler re-nudges once the gate is actually visible on wake.
                            g.nudgeGateKeyboard();
                            try {
                                g.getWindow().addFlags(android.view.WindowManager.LayoutParams.FLAG_TURN_SCREEN_ON);
                                android.os.PowerManager pm = (android.os.PowerManager)
                                        g.getSystemService(android.content.Context.POWER_SERVICE);
                                if (pm != null) {
                                    android.os.PowerManager.WakeLock wl = pm.newWakeLock(
                                            android.os.PowerManager.FULL_WAKE_LOCK
                                                    | android.os.PowerManager.ACQUIRE_CAUSES_WAKEUP, "nowhere:logoff-reveal");
                                    wl.acquire(3000);
                                }
                            } catch (Throwable t) { android.util.Log.w("NowhereChooser", "logoff wake", t); }
                        });
                        return;
                    }
                    // Still present. The FIRST removeUser (above) likely failed because the switch to the gate
                    // (user 0) hadn't COMPLETED -- you can't remove the CURRENT user, and switching to user 0 now
                    // takes several seconds (its keyguard going-away transition; measured ~8s on FP3). The old code
                    // assumed the switch "settled a poll apart" and removed once -> on a slow switch the remove
                    // failed, the user lingered, and the 12s fallback REBOOTED on every logoff. So RETRY removeUser
                    // each tick; once user 0 is actually foreground it takes, and the reboot fallback only fires on
                    // a genuine failure after a generous grace. (DIA-20260625-13)
                    try { dpm.removeUser(admin, android.os.UserHandle.of(uid)); } catch (Throwable ignore) {}
                    if (++tries >= 16) { // ~24s grace (covers the ~8s switch + the removal), then the sure wipe
                        android.util.Log.w("NowhereChooser", "user " + uid + " still present after retries -> reboot to wipe");
                        try { dpm.reboot(admin); } catch (Throwable t) { android.util.Log.w("NowhereChooser", "dpm.reboot", t); }
                        return;
                    }
                    h.postDelayed(this, 1500);
                }
            }, 1500);
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "doRemove", t);
        }
    }

    /** After a roamed user is removed, its per-user CE *system* storage (/data/system_ce/<uid>, misc_ce, …) is
     *  LEFT BEHIND -- removeUser doesn't destroy it on this FBE build -- so a REUSED user-id inherits the stale,
     *  key-mismatched dir and crashes system_server on unlock (AccountManager can't open accounts_ce.db, code 14
     *  SQLITE_CANTOPEN). Have the root daemon rm the orphaned per-uid dirs. Best-effort + idempotent; the uid is
     *  already gone so it can't race a live user, and the system recreates a fresh dir for the next user with that
     *  id. Called from the logoff reap (doRemove) and the boot-wipe (BootReceiver). (DIA-20260625-13) */
    static void cleanUserStorage(int uid) {
        if (uid <= 0) return;
        try { pollDaemon("CLEAN-STORAGE\n" + uid + "\n"); }
        catch (Throwable t) { android.util.Log.w("NowhereChooser", "cleanUserStorage", t); }
    }

    /**
     * #73 (DIA-20260701-04): while PARKED at the gate, crypto-shred any leftover roam user that isn't the one
     * resumable cold-locked session (keepUid). A roamed user is normally reaped on logoff (doRemove) or on
     * boot-wipe, but edge paths leave orphans: a login whose chooser process died after createRoamUser but
     * before the switch/blind-reap, a removeUser that silently no-op'd, or a cold-lock that was SUPERSEDED by a
     * newer session (only the latest .coldlock uid is tracked, so the older kept user is now unreachable). Left
     * alone these hold a slot (max 4 users) and, worse, keep an identity's /data on the device — the amnesiac
     * contract says a logged-off identity leaves nothing. This enforces "at most one kept roam user" (the
     * cold-lock), so there can never be two live users for a profile to seal over each other (no split-brain).
     *
     * SAFETY: caller passes keepUid ONLY from a DEFINITIVE GET-COLDLOCK answer (a valid uid, or -1 for "NONE" =
     * nothing to keep) — a failed query skips the sweep entirely, so we never reap a cold-lock we failed to
     * learn about. Runs only while truly parked (no live session / login / reap / resume in flight) and
     * re-checks those flags before each removeUser, so a login that starts mid-sweep can never lose its user.
     */
    private void reapOrphanSecondaryUsers(int keepUid) {
        if (sessionLive || signingOut || resumeBusy || loginInProgress) return; // only when truly parked at the gate
        try {
            android.app.admin.DevicePolicyManager dpm =
                    (android.app.admin.DevicePolicyManager) getSystemService(DEVICE_POLICY_SERVICE);
            android.content.ComponentName admin = new android.content.ComponentName(this, AdminReceiver.class);
            for (android.os.UserHandle uh : dpm.getSecondaryUsers(admin)) {
                int uid = uh.getIdentifier();
                if (uid == keepUid) continue; // the one resumable cold-locked session -> keep
                if (loginInProgress || sessionLive || signingOut) return; // a login/reap started mid-sweep -> stop at once
                android.util.Log.i("NowhereChooser", "#73 reap orphan roam user " + uid + " (keep=" + keepUid + ")");
                try { dpm.removeUser(admin, uh); }
                catch (Throwable t) { android.util.Log.w("NowhereChooser", "reap removeUser " + uid, t); }
                cleanUserStorage(uid); // rm the per-uid CE leftovers so a reused uid can't crash system_server
            }
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "reapOrphanSecondaryUsers", t);
        }
    }

    /** P3 cold-lock PHASE 2b (device owner, user 0; DIA-20260625-13): STOP the roamed user (NOT remove) so its
     *  CE key is evicted -> /data stays on disk FBE-encrypted, resumable. Then flush the kernel dentry/inode
     *  cache (root, via the daemon) so even a powered-on locked device is immediately ciphertext. The daemon's
     *  .coldlock marker lets the gate offer RESUME (P4). Unlike doRemove there's no reboot fallback: a leftover
     *  cold-locked user is the intended state, and a power-off boot-wipes it. */
    private static void doColdLock(Context ctx, final int uid) {
        try {
            final Context app = ctx.getApplicationContext();
            final android.app.admin.DevicePolicyManager dpm =
                    (android.app.admin.DevicePolicyManager) app.getSystemService(Context.DEVICE_POLICY_SERVICE);
            final android.content.ComponentName admin = new android.content.ComponentName(app, AdminReceiver.class);
            int r = -1;
            try { r = dpm.stopUser(admin, android.os.UserHandle.of(uid)); }
            catch (Throwable t) { android.util.Log.w("NowhereChooser", "coldlock stopUser", t); }
            android.util.Log.i("NowhereChooser", "cold-lock: stopUser " + uid + " = " + r);
            // The CE eviction is ASYNC after stopUser, so wait for it BEFORE flushing the cache -- dropping too
            // early leaves stale plaintext inodes in RAM (they'd survive until the next drop). Poll isUserUnlocked
            // off the main thread, then DROP-CACHES (root). Once the CE is locked, the cache only holds encrypted
            // names, so one drop is enough. (DIA-20260625-13)
            final android.os.UserManager um = (android.os.UserManager) app.getSystemService(Context.USER_SERVICE);
            new Thread(() -> {
                for (int i = 0; i < 20; i++) { // up to ~10s for the async CE eviction
                    try { Thread.sleep(500); } catch (InterruptedException ie) { break; }
                    boolean unlocked;
                    try { unlocked = um.isUserUnlocked(android.os.UserHandle.of(uid)); }
                    catch (Throwable t) { unlocked = false; }
                    if (!unlocked) break; // CE evicted
                }
                pollDaemon("DROP-CACHES\n");
                android.util.Log.i("NowhereChooser", "cold-lock: drop_caches after CE-evict (user " + uid + ")");
            }).start();
            signingOut = false;
            sessionLive = false; // locked, not live -> the reap watcher idles down
            synchronized (ChooserActivity.class) {
                if (sCredArmer != null) { try { app.unregisterReceiver(sCredArmer); } catch (Throwable ignore) {} sCredArmer = null; }
            }
            cancelIdleWatcher(app); // L4: the session is parked -> tear the idle alarm down
            armWipeWatcher(app, uid); // #3: start the hard-wipe backstop for the now-cold-locked (un-resumed) session
            // Repaint the gate (a cold-lock can fire with the screen off, like an idle auto-logoff).
            ChooserActivity g = sLiveGate;
            if (g != null) g.runOnUiThread(() -> {
                try {
                    g.applySigningOutUi();
                    g.getWindow().addFlags(android.view.WindowManager.LayoutParams.FLAG_TURN_SCREEN_ON);
                    android.os.PowerManager pm = (android.os.PowerManager) g.getSystemService(Context.POWER_SERVICE);
                    if (pm != null) {
                        android.os.PowerManager.WakeLock wl = pm.newWakeLock(
                                android.os.PowerManager.FULL_WAKE_LOCK
                                        | android.os.PowerManager.ACQUIRE_CAUSES_WAKEUP, "nowhere:coldlock-reveal");
                        wl.acquire(3000);
                    }
                } catch (Throwable t) { android.util.Log.w("NowhereChooser", "coldlock wake", t); }
            });
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "doColdLock", t);
        }
    }

    /** True iff the device owner still lists <uid> as a secondary user (its record -- and thus its /data --
     *  not yet removed). On any error report "not present" so we never reboot spuriously. */
    private static boolean userPresent(android.app.admin.DevicePolicyManager dpm,
                                       android.content.ComponentName admin, int uid) {
        try {
            for (android.os.UserHandle uh : dpm.getSecondaryUsers(admin)) {
                if (uh.getIdentifier() == uid) return true;
            }
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "getSecondaryUsers", t);
        }
        return false;
    }

    /** Like {@link #sendDaemon} but returns ALL reply lines (the daemon writes the payload then closes). #77:
     *  bounded connect via DaemonSocket. */
    private java.util.List<String> sendDaemonLines(String msg) {
        return DaemonSocket.roundTripLines(msg, 30000);
    }

    /** The default app set for a roamed profile, even a brand-new one with no captured list. A secondary
     *  Android user does NOT inherit system apps installed only for user 0 (notably /product + /system_ext
     *  apps), so each must be install-existing'd for the roamed user or it never appears in the session.
     *  Enumerated DYNAMICALLY -- every launchable SYSTEM app on this device -- so it is edition-agnostic and
     *  never goes stale: on the FP3 it picks up the LineageOS + curated F-Droid apps; on Endospore the
     *  GrapheneOS apps (Camera/Gallery/Calendar/Music/browser/...). SUPERSEDES the old hardcoded BASE_APPS,
     *  whose LineageOS package names (org.lineageos.aperture/glimpse/etar/twelve/recorder + the curated F-Droid
     *  set) don't exist on GrapheneOS, so install-existing silently no-op'd -> a ~9-app roamed session on
     *  Endospore (DIA-20260621). The default_workspace home layout places a few; the rest live in the drawer.
     *  The roamed app list (GET-APPS) installs on top, so user-added apps still roam. */
    private java.util.Set<String> baseApps() {
        java.util.Set<String> pkgs = new java.util.LinkedHashSet<>();
        android.content.Intent main = new android.content.Intent(android.content.Intent.ACTION_MAIN);
        main.addCategory(android.content.Intent.CATEGORY_LAUNCHER);
        for (android.content.pm.ResolveInfo ri :
                getPackageManager().queryIntentActivities(main, android.content.pm.PackageManager.MATCH_SYSTEM_ONLY)) {
            if (ri.activityInfo != null && ri.activityInfo.packageName != null) pkgs.add(ri.activityInfo.packageName);
        }
        return pkgs;
    }

    private boolean installExistingFor(String pkg, int uid) {
        try {
            return getPackageManager().installExistingPackageAsUser(pkg, uid)
                    == android.content.pm.PackageManager.INSTALL_SUCCEEDED;
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "install-existing " + pkg, t);
            return false;
        }
    }

    /** Reinstall the roamed identity's apps. The worker surfaced the sealed app list from the profile; the
     *  daemon hands it back via GET-APPS, and we install each for the user (device-local install-existing
     *  for now -- the cross-device code fetch from Aurora/F-Droid is the productionization). BASE_APPS land
     *  first, regardless of the captured list, so even a fresh profile gets the store. The su:s0 worker
     *  can't do this (pm hits the device-owner wall); the chooser, as device owner, can. */
    private void provisionRoamedApps(int uid) {
        java.util.Set<String> base = baseApps();
        for (String pkg : base) installExistingFor(pkg, uid);
        java.util.List<String> apps = sendDaemonLines("GET-APPS\n");
        int n = 0;
        for (String pkg : apps) { if (installExistingFor(pkg, uid)) n++; }
        android.util.Log.i("NowhereChooser",
                "provisioned " + n + "/" + apps.size() + " roamed apps + " + base.size() + " base (dynamic) -> user " + uid);
        // Now that the apps EXIST for this user, re-grant their roamed runtime permissions (location, etc.).
        // Those live in misc_de, outside the sealed dirs, so they don't roam with the app data; the worker
        // captured them at the last logout and pm-grants them here -- WITHOUT this every roamed login re-prompts
        // (e.g. Organic Maps re-asks for location every time). Must follow install-existing. (DIA-20260625-06)
        sendDaemonLines("GRANT-PERMS\n" + uid + "\n");
    }

    /** Re-apply the roamed-in profile's timezone + locale on login (DIA-20260616-58). The worker surfaced them
     *  from the sealed CE data; we GET-PREFS and apply -- TIMEZONE via the DEVICE OWNER
     *  (DevicePolicyManager.setTimeZone: live, no extra permission, no reboot) and LOCALE via the platform
     *  LocalePicker (platform-signed + CHANGE_CONFIGURATION). Both are system-GLOBAL, so applying from the
     *  user-0 gate just before the switch carries into the session the user lands in. Falls back to the baked
     *  ro.nowhere.default.* so a profile with NO saved prefs (e.g. a fresh CREATE) still lands deterministically
     *  on the device default instead of inheriting the previous user's zone. Best-effort: a failure must not
     *  break the login (the user is already roamed in by here). */
    private void applyRoamedPrefs() {
        String tz = null, loc = null, idle = null, wipe = null, raw = "";
        try {
            java.util.List<String> lines = sendDaemonLines("GET-PREFS\n");
            raw = android.text.TextUtils.join("|", lines);
            for (String l : lines) {
                if (l.startsWith("tz=")) tz = l.substring(3).trim();
                else if (l.startsWith("locale=")) loc = l.substring(7).trim();
                else if (l.startsWith("idle=")) idle = l.substring(5).trim();
                else if (l.startsWith("wipe=")) wipe = l.substring(5).trim();
            }
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "GET-PREFS", t);
        }
        // L5 (DIA-20260623-26): per-profile idle auto-logoff timeout. `idle=<minutes>` roams in the prefs;
        // "0" = Never (don't arm); unset -> the IDLE_LOGOFF_MS default. armIdleWatcher reads sIdleTimeoutMs.
        long idleMs = IDLE_LOGOFF_MS;
        try { if (idle != null && !idle.isEmpty()) idleMs = Long.parseLong(idle) * 60L * 1000L; } catch (Throwable ignore) {}
        sIdleTimeoutMs = idleMs;
        // #3 (DIA-20260626-03): per-profile hard-wipe backstop. `wipe=<hours>` roams; "0" = Never; unset -> the
        // HARD_WIPE_MS (12 h) default. armWipeWatcher reads sWipeTimeoutMs at cold-lock.
        long wipeMs = HARD_WIPE_MS;
        try { if (wipe != null && !wipe.isEmpty()) wipeMs = Long.parseLong(wipe) * 60L * 60L * 1000L; } catch (Throwable ignore) {}
        sWipeTimeoutMs = wipeMs;
        // TIMEZONE MODEL (DIA-20260616-60, design of record: docs/timezone-model.md). Timezone follows WHERE YOU
        // ARE -- AUTOMATIC by default, with an optional per-profile OVERRIDE -- so `tz` from the prefs is the
        // OVERRIDE (empty = automatic) and is NOT defaulted to the baked value. Locale follows WHO YOU ARE
        // (identity-pinned, roams), so it IS defaulted.
        String tzOverride = (tz == null) ? "" : tz;
        if (loc == null || loc.isEmpty()) loc = sysProp("ro.nowhere.default.locale", "en-US");
        android.util.Log.i("NowhereChooser",
                "applyRoamedPrefs: GET-PREFS=[" + raw + "] -> tzOverride=[" + tzOverride + "] loc=" + loc);
        try {
            android.app.admin.DevicePolicyManager dpm =
                    (android.app.admin.DevicePolicyManager) getSystemService(Context.DEVICE_POLICY_SERVICE);
            android.content.ComponentName admin = new android.content.ComponentName(this, AdminReceiver.class);
            if (!tzOverride.isEmpty()) {
                // OVERRIDE: the profile pins a specific zone. A device-owner setTimeZone is a MANUAL suggestion the
                // detector ignores while auto-detection is ON, so disable it first, then pin.
                dpm.setAutoTimeZoneEnabled(admin, false);
                dpm.setTimeZone(admin, tzOverride);
                android.util.Log.i("NowhereChooser", "applied timezone OVERRIDE " + tzOverride);
            } else {
                // AUTOMATIC (no override): pick the best available SEED so a device with NO resolved signal yet is
                // deterministic rather than stale, THEN enable auto-detection so NITZ (FP6 + SIM) / geolocation
                // take over whenever a signal exists. Seed priority: the tier-4 IP fallback (a coarse zone from the
                // public IP) when it resolves, else the baked market default. Auto stays ON either way, so a real
                // NITZ/geo fix always OVERRIDES the seed -- the IP zone is just a fast, better-than-baked first
                // guess (DIA-20260617-03, docs/timezone-model.md). This is the FP6-ready path: DIA-58 disabled auto
                // UNCONDITIONALLY, which would have SUPPRESSED NITZ on a SIM device.
                String def = sysProp("ro.nowhere.default.timezone", "America/New_York");
                String seed = def;
                String ipTz = resolveIpTz(); // tier-4: the daemon does the egress; "" if no net / unresolved
                if (!ipTz.isEmpty() && isKnownZone(ipTz)) {
                    seed = ipTz;
                    android.util.Log.i("NowhereChooser", "timezone IP fallback resolved " + ipTz);
                }
                dpm.setAutoTimeZoneEnabled(admin, false);
                dpm.setTimeZone(admin, seed);
                dpm.setAutoTimeZoneEnabled(admin, true);
                android.util.Log.i("NowhereChooser", "timezone AUTOMATIC (seeded " + seed + ", auto-detection on)");
            }
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "apply timezone", t);
        }
        if (loc != null && !loc.isEmpty()) {
            try {
                com.android.internal.app.LocalePicker.updateLocale(java.util.Locale.forLanguageTag(loc));
                android.util.Log.i("NowhereChooser", "applied roamed locale " + loc);
            } catch (Throwable t) {
                android.util.Log.w("NowhereChooser", "updateLocale " + loc, t);
            }
        }
    }

    /** Tier-4 IP fallback (docs/timezone-model.md): ask the daemon -- which owns the network egress -- for a
     *  coarse timezone derived from the device's public IP. Returns "" on any failure. Used ONLY as the
     *  AUTOMATIC seed when a profile has no override; applied with auto-detection still ON, so a real NITZ/geo
     *  fix overrides it. Best-effort: VPN-fragile + leaks coarse location, so it never wins over an actual fix. */
    private String resolveIpTz() {
        try {
            for (String l : sendDaemonLines("RESOLVE-IP-TZ\n")) {
                if (l.startsWith("tz=")) return l.substring(3).trim();
            }
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "RESOLVE-IP-TZ", t);
        }
        return "";
    }

    /** True iff zone is a timezone ID the platform knows -- guards setTimeZone against a bogus resolver reply. */
    private boolean isKnownZone(String zone) {
        if (zone == null || zone.isEmpty()) return false;
        for (String id : java.util.TimeZone.getAvailableIDs()) {
            if (id.equals(zone)) return true;
        }
        return false;
    }

    /** SystemProperties.get with a guaranteed non-empty fallback (a denied/empty read returns def). */
    private String sysProp(String key, String def) {
        try {
            String v = android.os.SystemProperties.get(key, "");
            return (v == null || v.isEmpty()) ? def : v;
        } catch (Throwable t) {
            return def;
        }
    }

    /** Create + start a fresh ephemeral Android user for this identity (device-owner DPM). Returns id or -1. */
    private int createRoamUser(String name) {
        try {
            android.app.admin.DevicePolicyManager dpm =
                    (android.app.admin.DevicePolicyManager) getSystemService(Context.DEVICE_POLICY_SERVICE);
            android.content.ComponentName admin = new android.content.ComponentName(this, AdminReceiver.class);
            // The user NAME is what the system's "Switching to <name>" dialog shows -- use the bare profile name.
            // P1 (DIA-20260625-11, docs/resumable-session.md): roamed users are now NON-EPHEMERAL, so a future
            // idle-LOCK (stopUser, not removeUser) FBE-locks /data at rest instead of deleting it. Logoff still
            // crypto-shreds via the reap's explicit dpm.removeUser (doRemove), and a blind login removes its empty
            // user too (stopRoamUser -> removeUser). NB until P2 (boot-wipe) lands, a power-off leaves the user's
            // /data ENCRYPTED-at-rest (CE-locked), not deleted -- the resumable model's behaviour, not a leak.
            android.os.UserHandle uh = dpm.createAndManageUser(admin, name, admin, null,
                    android.app.admin.DevicePolicyManager.SKIP_SETUP_WIZARD);
            if (uh == null) return -1;
            dpm.startUserInBackground(admin, uh);
            return uh.getIdentifier();
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "createRoamUser", t);
            return -1;
        }
    }

    /** Bring user <uid> to the foreground (ActivityManager.switchUser, device-owner privilege). */
    private boolean switchToUser(int uid) {
        try {
            android.app.ActivityManager am =
                    (android.app.ActivityManager) getSystemService(ACTIVITY_SERVICE);
            return am.switchUser(android.os.UserHandle.of(uid));
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "switchToUser", t);
            return false;
        }
    }

    /** Remove a leftover roamed user (blind-login cleanup). Non-ephemeral (P1) -> a stopUser would leave the empty
     *  user on disk, so removeUser outright: a wrong-password attempt must leave nothing behind. (DIA-20260625-11) */
    private void stopRoamUser(int uid) {
        try {
            android.app.admin.DevicePolicyManager dpm =
                    (android.app.admin.DevicePolicyManager) getSystemService(Context.DEVICE_POLICY_SERVICE);
            android.content.ComponentName admin = new android.content.ComponentName(this, AdminReceiver.class);
            dpm.removeUser(admin, android.os.UserHandle.of(uid));
        } catch (Throwable ignore) {
        }
    }

    /**
     * Make a freshly-roamed user land clean: disable the setup wizard (it ignores DPM's
     * SKIP_SETUP_WIZARD and would otherwise greet the user with a stock welcome screen), and force-stop the
     * apps that came up on in-flight data during the restore so they re-init ONCE on final data instead of
     * crash-looping through the window. We do this as the device owner with cross-user permissions -- the
     * su:s0 worker can't (raw pm/am hit the device-owner wall), but this app context can.
     */
    private static final String[] ROAM_CRASHERS = {"com.android.launcher3",
            "com.android.providers.contacts", "com.android.providers.media.module",
            "com.android.providers.media"};

    /** Enable (DEFAULT) or disable (DISABLED_USER) the crash-prone apps for the roamed user (cross-user,
     *  device-owner). Disabling before the restore stops them + blocks relaunch; re-enabling after lets
     *  them come up clean on the restored data. */
    private void setRoamCrashersEnabled(int uid, boolean enabled) {
        int state = enabled ? android.content.pm.PackageManager.COMPONENT_ENABLED_STATE_DEFAULT
                            : android.content.pm.PackageManager.COMPONENT_ENABLED_STATE_DISABLED_USER;
        android.content.pm.PackageManager pm;
        try { pm = createContextAsUser(android.os.UserHandle.of(uid), 0).getPackageManager(); }
        catch (Throwable t) { android.util.Log.w("NowhereChooser", "ctx-as-user", t); return; }
        for (String p : ROAM_CRASHERS) {
            try { pm.setApplicationEnabledSetting(p, state, 0); }
            catch (Throwable t) { android.util.Log.w("NowhereChooser", "setEnabled " + p, t); }
        }
    }

    /** After the restore: re-enable the apps quiesced before it (they now launch clean on final data), and
     *  keep LineageOS's setup wizard off (it ignores DPM SKIP_SETUP_WIZARD). Device-owner cross-user ops. */
    private void prepRoamedUser(int uid) {
        setRoamCrashersEnabled(uid, true);
        try {
            createContextAsUser(android.os.UserHandle.of(uid), 0).getPackageManager()
                    .setApplicationEnabledSetting("org.lineageos.setupwizard",
                            android.content.pm.PackageManager.COMPONENT_ENABLED_STATE_DISABLED_USER, 0);
        } catch (Throwable t) {
            android.util.Log.w("NowhereChooser", "disable setupwizard", t);
        }
    }

    /** The dispersing-spore motif from the boot animation, drawn to scale (viewBox 66x34). */
    private static class SporeView extends View {
        private final Paint p = new Paint(Paint.ANTI_ALIAS_FLAG);
        SporeView(Context c) { super(c); }
        @Override protected void onDraw(Canvas canvas) {
            float sx = getWidth() / 66f, sy = getHeight() / 34f, s = Math.min(sx, sy);
            dot(canvas, 33, 27, 4.2f, 0xFF4FD6AC, 255, sx, sy, s);
            dot(canvas, 22, 18, 2.6f, 0xFFEAF1EE, 217, sx, sy, s);
            dot(canvas, 44, 16, 3.0f, 0xFF4FD6AC, 179, sx, sy, s);
            dot(canvas, 14,  9, 2.0f, 0xFF4FD6AC, 128, sx, sy, s);
            dot(canvas, 52,  8, 2.0f, 0xFFEAF1EE, 102, sx, sy, s);
            dot(canvas, 34,  5, 1.6f, 0xFF4FD6AC, 115, sx, sy, s);
        }
        private void dot(Canvas c, float x, float y, float r, int color, int a, float sx, float sy, float s) {
            p.setColor(color);
            p.setAlpha(a);
            c.drawCircle(x * sx, y * sy, r * s, p);
        }
    }
}
