package com.diaspore.chooser;

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
 * Diaspore blind-login chooser (P3.1, FP3 integration).
 *
 * Collects a profile name + passphrase and hands them to the ROOT login daemon over the AF_UNIX
 * socket /dev/socket/diaspore_login (created by init; the daemon is `diaspore_agent login-daemon`).
 * The daemon restores that profile's working set into the roaming tmpfs (/data/diaspore/state) and
 * replies "OK <n>" or "BLANK". The passphrase is sent in-memory over the socket -- it never touches
 * disk or any argv. An app can't do the restore itself (root-owned tmpfs / baked creds / S3 net), so
 * this socket is the privilege boundary.
 *
 * BLIND LOGIN: wrong passphrase OR unknown profile renders the SAME blank "-", indistinguishable, so
 * a hidden/duress profile is indistinguishable from a typo. A correct (name, passphrase) unlocks it.
 *
 * The UI is built in code (no res/ resources) and styled to the Diaspore brand: dark canvas, the
 * "diaspore" wordmark + a dispersing-spore motif, a teal accent, two blind-login fields (no profile
 * list -- the absence is the security property), and the amnesiac promise as a footer.
 */
public class ChooserActivity extends Activity {
    private static final String SOCKET = "diaspore_login";

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
    private TextView toggle; // the create-profile / sign-in mode switch (locked during restore)
    private TextView gear; // top-right Settings gear; accent-tinted when an OS update is available (DIA-20260618-04)
    private android.content.BroadcastReceiver screenOffRx; // auto-install OTA when parked (DIA-20260618-06)
    private Button unlockBtn;
    private View gateView;   // the main gate layout, kept so the recover/recovery-code screens can swap back
    private android.widget.ProgressBar progressBar; // determinate restore bar, driven by streamed progress
    private boolean createMode = false; // false = sign in; true = create a new profile
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
        title.setText("diaspore");
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
                android.util.Log.w("DiasporeChooser", "emergency dial", t);
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
            String n = profileField.getText().toString();
            String p = passField.getText().toString();
            if (createMode) {
                if (p.isEmpty() || !p.equals(confirmField.getText().toString())) {
                    result.setTextColor(MUTED);
                    result.setText(p.isEmpty() ? "—" : "passphrases don't match");
                    return;
                }
                create(n, p);
            } else {
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
        // First-run hint: with no store configured the device can't create/unlock yet -> point to Store settings.
        new Thread(() -> {
            if (sendDaemonLines("GET-STORE\n").contains("configured=no")) showStoreHint();
        }).start();
        // Name the primary user "Diaspore" -- it ships unnamed, so the system shows the generic "Owner"
        // (e.g. on the emergency-info card). Device-owner + platform-signed, so setUserName is permitted.
        try {
            android.os.UserManager um = (android.os.UserManager) getSystemService(USER_SERVICE);
            if (android.os.UserHandle.myUserId() == 0 && !"Diaspore".equals(um.getUserName())) {
                um.setUserName("Diaspore");
            }
        } catch (Throwable t) {
            android.util.Log.w("DiasporeChooser", "setUserName", t);
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
                // managed-device disclosure + an owner-info line over the teal Diaspore wallpaper. Set both to the
                // Diaspore identity so the rare keyguard is on-brand. Device-owner APIs (idempotent), set on the
                // user-0 gate at device-owner scope, so it applies to the keyguard device-wide. (A full spore
                // GRAPHIC on the keyguard would need a per-user runtime FLAG_LOCK wallpaper on each ephemeral
                // roamed user -- no build-time default lock wallpaper exists in AOSP -- so that's deferred.)
                try { dpm.setOrganizationName(admin, "diaspore"); } catch (Throwable t) {
                    android.util.Log.w("DiasporeChooser", "setOrganizationName", t);
                }
                try { dpm.setDeviceOwnerLockScreenInfo(admin, "diaspore · your phone, nowhere"); }
                catch (Throwable t) { android.util.Log.w("DiasporeChooser", "setDeviceOwnerLockScreenInfo", t); }
            }
        } catch (Throwable t) {
            android.util.Log.w("DiasporeChooser", "clearAffiliationIds", t);
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
        lockGate(true); // boot-launched gate -> kiosk-lock until we confirm there's no active session
        applySigningOutUi(); // if a logoff's reap is still finishing, show "Signing out…" (the gate looks frozen)
        // Privacy: a gate AT REST must not hold the Wi-Fi-scan location grant. If we still hold FINE_LOCATION
        // from an earlier scan (e.g. a session that connected then logged out, or a leftover across reboot),
        // drop it now. Revoking restarts the gate, but we're foregrounding the IDLE gate so that just re-shows
        // it -- a one-time, self-healing cycle (after the restart the grant is gone, so this is a no-op).
        // onResume only fires on real foregrounding (boot / post-logout / post-dialer), NOT on the in-activity
        // Wi-Fi screen swaps, so it never kills the gate mid-onboarding (Back/connect revoke those directly).
        if (checkSelfPermission(android.Manifest.permission.ACCESS_FINE_LOCATION)
                == android.content.pm.PackageManager.PERMISSION_GRANTED) {
            revokeScanPerms();
        }
        // launcher3 is the home now; the chooser is only launched (by BootReceiver) as the boot gate.
        // If a session is somehow already active (a spurious relaunch over a logged-in launcher), don't
        // re-gate -- drop the lock and finish, revealing launcher3 (the home).
        new Thread(() -> {
            if ("ACTIVE".equals(queryStatus())) {
                runOnUiThread(() -> { lockGate(false); finish(); });
            }
        }).start();
        checkForOta(); // accent the Settings gear if a newer OS is published
        // Auto-install OTA when parked (DIA-20260618-06): if the user enabled it in Settings, install while the
        // device is charging AND the screen is off (so it never interrupts active use). Registered once; on
        // screen-off it re-checks the conditions and only then kicks the apply.
        if (screenOffRx == null) {
            screenOffRx = new android.content.BroadcastReceiver() {
                @Override public void onReceive(android.content.Context c, Intent i) {
                    new Thread(() -> {
                        if ("1".equals(daemonCmd("GET-OTA-AUTO")) && isCharging()
                                && daemonCmd("OTA-STATUS").startsWith("AVAIL")) {
                            daemonCmd("OTA-APPLY");
                        }
                    }).start();
                }
            };
            registerReceiver(screenOffRx, new android.content.IntentFilter(Intent.ACTION_SCREEN_OFF),
                    android.content.Context.RECEIVER_NOT_EXPORTED);
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
    }

    /** True when the device is on external power (AC/USB/wireless). Read from the sticky battery-changed
     *  broadcast -- no permission needed -- so the auto-OTA watcher only installs while charging. */
    private boolean isCharging() {
        Intent bs = registerReceiver(null,
                new android.content.IntentFilter(Intent.ACTION_BATTERY_CHANGED));
        if (bs == null) return false;
        return bs.getIntExtra(android.os.BatteryManager.EXTRA_PLUGGED, 0) != 0;
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
                    android.util.Log.i("DiasporeChooser", "gained focus + not locked -> retry lockGate");
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
        // letting them reach PhoneWindow's default handling CRASHES the gate: our confined diaspore_chooser
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
            android.util.Log.i("DiasporeChooser", "lockGate(" + lock + ") owner=" + owner);
            if (owner) {
                if (lock) {
                    dpm.setLockTaskPackages(new android.content.ComponentName(this, AdminReceiver.class),
                            new String[]{getPackageName(), "com.android.phone", "com.android.emergency"}); // emergency dialer + info card at the locked gate
                    startLockTask();
                    int st = ((android.app.ActivityManager) getSystemService(ACTIVITY_SERVICE)).getLockTaskModeState();
                    android.util.Log.i("DiasporeChooser", "startLockTask -> state=" + st);
                    // startLockTask throws/no-ops if our task isn't foreground (e.g. SetupWizard won the boot
                    // race -> "Invalid task, not in foreground"). If it didn't engage we DON'T give up: the
                    // retry fires from onWindowFocusChanged once the gate actually becomes the focused task.
                    if (st != android.app.ActivityManager.LOCK_TASK_MODE_LOCKED) {
                        android.util.Log.w("DiasporeChooser", "lock not engaged (task not foreground?) -> will retry on focus");
                    }
                } else {
                    stopLockTask();
                }
            }
        } catch (Exception e) {
            android.util.Log.w("DiasporeChooser", "lockGate err", e);
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

    /** Send a one-line command to the login daemon and return its first reply line ("" on any error). */
    private String daemonCmd(String cmd) {
        try {
            LocalSocket s = new LocalSocket();
            s.connect(new LocalSocketAddress(SOCKET, LocalSocketAddress.Namespace.RESERVED));
            s.setSoTimeout(8000); // OTA-STATUS hits the store (a network GET), so allow more than a local probe
            OutputStream os = s.getOutputStream();
            os.write((cmd + "\n").getBytes("UTF-8"));
            os.flush();
            BufferedReader br = new BufferedReader(new InputStreamReader(s.getInputStream(), "UTF-8"));
            String line = br.readLine();
            s.close();
            return line == null ? "" : line.trim();
        } catch (Exception e) {
            return "";
        }
    }

    /** Ask the daemon whether a session is active (the tmpfs roaming state is populated). */
    private String queryStatus() {
        return daemonCmd("STATUS");
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

        // Opt-in auto-install (DIA-20260618-06): when on, the gate installs a published update on the next
        // screen-off while charging (see the ACTION_SCREEN_OFF watcher in onResume) -- never mid-use. The
        // flag is device-level (/data/diaspore/ota-auto via the daemon), default off.
        final android.widget.Switch autoSw = new android.widget.Switch(this);
        autoSw.setText("Install automatically while charging");
        autoSw.setTextColor(MUTED);
        autoSw.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        LinearLayout.LayoutParams asp = new LinearLayout.LayoutParams(MATCH, WRAP);
        asp.topMargin = dp(20);
        ll.addView(autoSw, asp);
        new Thread(() -> {
            final boolean on = "1".equals(daemonCmd("GET-OTA-AUTO"));
            runOnUiThread(() -> {
                autoSw.setChecked(on); // set state first, THEN wire the listener so loading doesn't write back
                autoSw.setOnCheckedChangeListener((b, c) ->
                        new Thread(() -> daemonCmd("SET-OTA-AUTO\n" + (c ? "1" : "0"))).start());
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
            swMsg.setText("Updating Diaspore… the phone will restart when it's done.");
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
                android.util.Log.i("DiasporeChooser", "handoff -> " + pkg + "/" + ri.activityInfo.name);
                return;
            } catch (Exception e) {
                android.util.Log.w("DiasporeChooser", "home not launchable: " + pkg + " -> " + e);
            }
        }
        android.util.Log.w("DiasporeChooser", "no launcher to hand off to (" + ris.size() + " home candidates)");
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
                if (!cr.startsWith("OK")) { hideRestore(); showBlank(); return; }
                String rec = parseRecovery(cr); // "OK 0 RECOVERY <12 words>" -> show the code once, then continue
                if (rec != null) { showRecoveryCode(rec); showRestoring(name); }
            }
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
                runOnUiThread(() -> { progressBar.setProgress(100); result.setText("Finishing up…"); });
                prepRoamedUser(uid); // land clean: re-enable the quiesced apps + keep the setup wizard off
                provisionRoamedApps(uid); // reinstall the roamed identity's apps (device-local, for now)
                applyRoamedPrefs(); // re-apply this profile's roamed timezone + locale (or the baked default)
                // Drop the kiosk, switch into the roamed user, finish -- all on the UI thread, in order.
                sessionLive = true; // a session is now live -> the reap watcher polls fast for a quick logoff (DIA-20260618-07)
                runOnUiThread(() -> { lockGate(false); switchToUser(uid); finish(); });
            } else {
                if (uid >= 0) stopRoamUser(uid); // blind: no access; gate/reboot reaps the empty user
                hideRestore();
                if (reply.startsWith("NOSTORE")) showStoreHint(); else showBlank(); // no store -> point to Settings
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

    /** The Diaspore accent button (teal, rounded), matching the gate's Unlock. */
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
        done.setOnClickListener(v -> latch.countDown());

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
     *  via the daemon's SET-STORE (root, to /data/diaspore/diaspore.conf, applied live); GET-STORE pre-fills
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
        cancel.setOnClickListener(v -> setContentView(gateView));

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
            runOnUiThread(() -> {
                if (m.get("endpoint") != null) endpointF.setText(m.get("endpoint"));
                if (m.get("bucket") != null) bucketF.setText(m.get("bucket"));
                String rg = m.get("region");
                if (rg != null && !rg.equals("auto")) regionF.setText(rg);
                if ("yes".equals(m.get("keyset"))) {
                    msg.setTextColor(MUTED);
                    msg.setText("a key is already set — re-enter both keys to change it");
                } else {
                    msg.setTextColor(ACCENT);
                    msg.setText("no store yet — set one up to create or unlock a profile");
                }
            });
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
                        setContentView(gateView);
                        result.setTextColor(ACCENT);
                        result.setText("store saved");
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
        back.setOnClickListener(v -> { setContentView(gateView); revokeScanPerms(); });

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
                        setContentView(gateView);
                        result.setTextColor(ACCENT);
                        result.setText("Wi-Fi connected — you can sign in now");
                    } else {
                        msg.setTextColor(MUTED);
                        msg.setText(r.startsWith("ERR ") ? r.substring(4) : r);
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
            android.util.Log.w("DiasporeChooser", "connectedSsid", e);
            return null;
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
            // Device owner self-grants the scan perms + enables location for the scan. These are handed back
            // by revokeScanPerms() when the user LEAVES the Wi-Fi screen (NOT here): revoking a held runtime
            // permission RESTARTS this process, which would kill the gate right after the scan and throw away
            // the scanned-network list. Granting does NOT restart, so granting here is safe. NOTE:
            // NEARBY_WIFI_DEVICES alone is NOT enough on this build -- getScanResults() silently returns empty
            // unless the caller also holds ACCESS_FINE_LOCATION (+ location on). So grant both.
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
            android.util.Log.w("DiasporeChooser", "wifi scan", e);
        }
        return out;
    }

    /** Hand back the location access the Wi-Fi scan needed. Revoking a runtime permission the app currently
     *  holds RESTARTS this process, so this must be called only when LEAVING the Wi-Fi screen back to the gate
     *  (Back / after a successful connect) -- a gate restart there just re-shows the gate, which is where the
     *  user is headed anyway. Called from those two exits so a kiosk gate AT REST never holds location:
     *  FINE_LOCATION/NEARBY are dropped (re-granted on the next scan) and the location toggle is turned back
     *  off. FINE_LOCATION stays *declared* in the manifest -- only the runtime grant is dropped. */
    private void revokeScanPerms() {
        try {
            android.app.admin.DevicePolicyManager dpm =
                    (android.app.admin.DevicePolicyManager) getSystemService(DEVICE_POLICY_SERVICE);
            android.content.ComponentName admin = new android.content.ComponentName(this, AdminReceiver.class);
            if (dpm == null || !dpm.isDeviceOwnerApp(getPackageName())) return;
            for (String perm : new String[]{android.Manifest.permission.ACCESS_FINE_LOCATION,
                    "android.permission.NEARBY_WIFI_DEVICES"}) {
                try {
                    dpm.setPermissionGrantState(admin, getPackageName(), perm,
                            android.app.admin.DevicePolicyManager.PERMISSION_GRANT_STATE_DENIED);
                } catch (Throwable ignore) {}
            }
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
            android.util.Log.w("DiasporeChooser", "wifi connect", e);
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
        try {
            LocalSocket s = new LocalSocket();
            s.connect(new LocalSocketAddress(SOCKET, LocalSocketAddress.Namespace.RESERVED));
            s.setSoTimeout(120000); // restore does S3 I/O; streamed progress keeps the read alive
            OutputStream os = s.getOutputStream();
            os.write(("ROAM-IN\n" + name + "\n" + pw + "\n" + uid + "\n").getBytes("UTF-8"));
            os.flush();
            BufferedReader br = new BufferedReader(new InputStreamReader(s.getInputStream(), "UTF-8"));
            String line, verdict = "";
            while ((line = br.readLine()) != null) {
                line = line.trim();
                if (line.startsWith("PROGRESS ")) {
                    onRestoreProgress(line.substring("PROGRESS ".length()));
                } else if (!line.isEmpty()) {
                    verdict = line;
                    break;
                }
            }
            s.close();
            return verdict;
        } catch (Exception e) {
            android.util.Log.w("DiasporeChooser", "roamInStreaming", e);
            return "";
        }
    }

    /** Drive the bar from a "<phase> <done> <total>" progress line. */
    private void onRestoreProgress(String s) {
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
        try {
            LocalSocket s = new LocalSocket();
            s.connect(new LocalSocketAddress(SOCKET, LocalSocketAddress.Namespace.RESERVED));
            s.setSoTimeout(90000); // roam restore/seal does S3 I/O -> allow time
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

    private static volatile boolean reapWatcherStarted = false;
    private static volatile boolean signingOut = false;          // a no-reboot logoff's reap is still finishing
    private static volatile boolean sessionLive = false;         // a roamed session is logged in -> poll fast for a quick logoff (DIA-20260618-07)
    private static volatile ChooserActivity sLiveGate = null;     // the foreground gate, for the reap watcher to update

    /**
     * Reboot-free logoff, device-owner half. LogoffActivity (on the roamed secondary user) tells the daemon
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
                        android.util.Log.i("DiasporeChooser", "reap watcher: SWITCH (screenOn="
                                + (pm != null && pm.isInteractive()) + ")");
                        main.post(() -> doSwitch(app, uid));
                    } catch (NumberFormatException ignore) {
                    }
                } else if (reply.startsWith("REAP ")) { // phase 2: remove the user (the daemon has sealed it)
                    try {
                        final int uid = Integer.parseInt(reply.substring(5).trim());
                        main.post(() -> doRemove(app, uid));
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
        }, "diaspore-reap-watcher");
        t.setDaemon(true);
        t.start();
    }

    /** One short request/reply to the daemon (used by the reap watcher's poll). */
    private static String pollDaemon(String msg) {
        try {
            android.net.LocalSocket s = new android.net.LocalSocket();
            s.connect(new android.net.LocalSocketAddress(SOCKET, android.net.LocalSocketAddress.Namespace.RESERVED));
            s.setSoTimeout(5000);
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
            android.util.Log.i("DiasporeChooser", "no-reboot logoff: switched to gate; user " + uid + " backgrounded, awaiting seal");
        } catch (Throwable t) {
            android.util.Log.w("DiasporeChooser", "doSwitch", t);
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
            catch (Throwable t) { ok = false; android.util.Log.w("DiasporeChooser", "removeUser", t); }
            android.util.Log.i("DiasporeChooser", "no-reboot logoff: removeUser " + uid + " ok=" + ok);
            // removeUser usually returns false yet schedules+completes the removal, so verify the REAL state
            // (the user record -- deleted together with its /data) by polling, and reboot only if it is still
            // there after a grace window. Closes the hole where a logged-off profile's data could survive a
            // failed in-place wipe.
            h.postDelayed(new Runnable() {
                int tries = 0;
                @Override public void run() {
                    if (!userPresent(dpm, admin, uid)) {
                        android.util.Log.i("DiasporeChooser", "no-reboot logoff: user " + uid + " wiped");
                        signingOut = false; // reap done -> drop the "Signing out…" notice, re-enable the gate
                        sessionLive = false; // no session left -> the reap watcher can idle down (DIA-20260618-07)
                        ChooserActivity g = sLiveGate;
                        if (g != null) g.runOnUiThread(g::applySigningOutUi);
                        return;
                    }
                    if (++tries >= 8) { // ~12s grace, then fall back to the sure wipe
                        android.util.Log.w("DiasporeChooser", "user " + uid + " still present -> reboot to wipe");
                        try { dpm.reboot(admin); } catch (Throwable t) { android.util.Log.w("DiasporeChooser", "dpm.reboot", t); }
                        return;
                    }
                    h.postDelayed(this, 1500);
                }
            }, 1500);
        } catch (Throwable t) {
            android.util.Log.w("DiasporeChooser", "doRemove", t);
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
            android.util.Log.w("DiasporeChooser", "getSecondaryUsers", t);
        }
        return false;
    }

    /** Like {@link #sendDaemon} but returns ALL reply lines (the daemon writes the payload then closes). */
    private java.util.List<String> sendDaemonLines(String msg) {
        java.util.ArrayList<String> lines = new java.util.ArrayList<>();
        try {
            LocalSocket s = new LocalSocket();
            s.connect(new LocalSocketAddress(SOCKET, LocalSocketAddress.Namespace.RESERVED));
            s.setSoTimeout(30000);
            OutputStream os = s.getOutputStream();
            os.write(msg.getBytes("UTF-8"));
            os.flush();
            BufferedReader br = new BufferedReader(new InputStreamReader(s.getInputStream(), "UTF-8"));
            String line;
            while ((line = br.readLine()) != null) { line = line.trim(); if (!line.isEmpty()) lines.add(line); }
            s.close();
        } catch (Exception e) {
            android.util.Log.w("DiasporeChooser", "sendDaemonLines", e);
        }
        return lines;
    }

    /** Base apps every roamed profile gets, even a brand-new one with no captured list. A secondary Android
     *  user does NOT inherit system apps installed only for user 0 (notably /product + /system_ext apps), so
     *  each must be install-existing'd for the roamed user or it never appears in the session. Beyond the
     *  store (F-Droid), this is the curated default set so a roamed session is a usable phone (DIA-20260617-07):
     *  the DIA-06 default apps + the core LineageOS apps (Camera/Clock/Gallery/Calculator/Calendar/Music/
     *  Recorder), which otherwise are absent from roamed sessions. Jelly (Fennec is the browser) + AudioFX
     *  (niche EQ) are deliberately omitted. The default_workspace home layout places these; the rest live in
     *  the drawer. The roamed app list (GET-APPS) is installed on top, so user-added apps still roam. */
    private static final String[] BASE_APPS = {
        "org.fdroid.fdroid",
        // DIA-06 default apps (privacy-first, no-GApps)
        "app.organicmaps", "eu.siacs.conversations", "org.breezyweather", "org.mozilla.fennec_fdroid",
        "com.beemdevelopment.aegis", "org.schabi.newpipe", "de.danoeh.antennapod",
        "me.hackerchick.catima", "com.omgodse.notally",
        // Core LineageOS apps (don't auto-install for secondary users)
        "org.lineageos.aperture", "com.android.deskclock", "org.lineageos.glimpse",
        "com.android.calculator2", "org.lineageos.etar", "org.lineageos.twelve", "org.lineageos.recorder",
    };

    private boolean installExistingFor(String pkg, int uid) {
        try {
            return getPackageManager().installExistingPackageAsUser(pkg, uid)
                    == android.content.pm.PackageManager.INSTALL_SUCCEEDED;
        } catch (Throwable t) {
            android.util.Log.w("DiasporeChooser", "install-existing " + pkg, t);
            return false;
        }
    }

    /** Reinstall the roamed identity's apps. The worker surfaced the sealed app list from the profile; the
     *  daemon hands it back via GET-APPS, and we install each for the user (device-local install-existing
     *  for now -- the cross-device code fetch from Aurora/F-Droid is the productionization). BASE_APPS land
     *  first, regardless of the captured list, so even a fresh profile gets the store. The su:s0 worker
     *  can't do this (pm hits the device-owner wall); the chooser, as device owner, can. */
    private void provisionRoamedApps(int uid) {
        for (String pkg : BASE_APPS) installExistingFor(pkg, uid);
        java.util.List<String> apps = sendDaemonLines("GET-APPS\n");
        int n = 0;
        for (String pkg : apps) { if (installExistingFor(pkg, uid)) n++; }
        android.util.Log.i("DiasporeChooser",
                "provisioned " + n + "/" + apps.size() + " roamed apps + " + BASE_APPS.length + " base -> user " + uid);
    }

    /** Re-apply the roamed-in profile's timezone + locale on login (DIA-20260616-58). The worker surfaced them
     *  from the sealed CE data; we GET-PREFS and apply -- TIMEZONE via the DEVICE OWNER
     *  (DevicePolicyManager.setTimeZone: live, no extra permission, no reboot) and LOCALE via the platform
     *  LocalePicker (platform-signed + CHANGE_CONFIGURATION). Both are system-GLOBAL, so applying from the
     *  user-0 gate just before the switch carries into the session the user lands in. Falls back to the baked
     *  ro.diaspore.default.* so a profile with NO saved prefs (e.g. a fresh CREATE) still lands deterministically
     *  on the device default instead of inheriting the previous user's zone. Best-effort: a failure must not
     *  break the login (the user is already roamed in by here). */
    private void applyRoamedPrefs() {
        String tz = null, loc = null, raw = "";
        try {
            java.util.List<String> lines = sendDaemonLines("GET-PREFS\n");
            raw = android.text.TextUtils.join("|", lines);
            for (String l : lines) {
                if (l.startsWith("tz=")) tz = l.substring(3).trim();
                else if (l.startsWith("locale=")) loc = l.substring(7).trim();
            }
        } catch (Throwable t) {
            android.util.Log.w("DiasporeChooser", "GET-PREFS", t);
        }
        // TIMEZONE MODEL (DIA-20260616-60, design of record: docs/timezone-model.md). Timezone follows WHERE YOU
        // ARE -- AUTOMATIC by default, with an optional per-profile OVERRIDE -- so `tz` from the prefs is the
        // OVERRIDE (empty = automatic) and is NOT defaulted to the baked value. Locale follows WHO YOU ARE
        // (identity-pinned, roams), so it IS defaulted.
        String tzOverride = (tz == null) ? "" : tz;
        if (loc == null || loc.isEmpty()) loc = sysProp("ro.diaspore.default.locale", "en-US");
        android.util.Log.i("DiasporeChooser",
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
                android.util.Log.i("DiasporeChooser", "applied timezone OVERRIDE " + tzOverride);
            } else {
                // AUTOMATIC (no override): pick the best available SEED so a device with NO resolved signal yet is
                // deterministic rather than stale, THEN enable auto-detection so NITZ (FP6 + SIM) / geolocation
                // take over whenever a signal exists. Seed priority: the tier-4 IP fallback (a coarse zone from the
                // public IP) when it resolves, else the baked market default. Auto stays ON either way, so a real
                // NITZ/geo fix always OVERRIDES the seed -- the IP zone is just a fast, better-than-baked first
                // guess (DIA-20260617-03, docs/timezone-model.md). This is the FP6-ready path: DIA-58 disabled auto
                // UNCONDITIONALLY, which would have SUPPRESSED NITZ on a SIM device.
                String def = sysProp("ro.diaspore.default.timezone", "America/New_York");
                String seed = def;
                String ipTz = resolveIpTz(); // tier-4: the daemon does the egress; "" if no net / unresolved
                if (!ipTz.isEmpty() && isKnownZone(ipTz)) {
                    seed = ipTz;
                    android.util.Log.i("DiasporeChooser", "timezone IP fallback resolved " + ipTz);
                }
                dpm.setAutoTimeZoneEnabled(admin, false);
                dpm.setTimeZone(admin, seed);
                dpm.setAutoTimeZoneEnabled(admin, true);
                android.util.Log.i("DiasporeChooser", "timezone AUTOMATIC (seeded " + seed + ", auto-detection on)");
            }
        } catch (Throwable t) {
            android.util.Log.w("DiasporeChooser", "apply timezone", t);
        }
        if (loc != null && !loc.isEmpty()) {
            try {
                com.android.internal.app.LocalePicker.updateLocale(java.util.Locale.forLanguageTag(loc));
                android.util.Log.i("DiasporeChooser", "applied roamed locale " + loc);
            } catch (Throwable t) {
                android.util.Log.w("DiasporeChooser", "updateLocale " + loc, t);
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
            android.util.Log.w("DiasporeChooser", "RESOLVE-IP-TZ", t);
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
            // The user NAME is what the system's "Switching to <name>" dialog shows -- use the bare profile
            // name (not "diaspore_"+name). Safe: the ephemeral user is reaped by uid + MAKE_USER_EPHEMERAL,
            // never by this label.
            android.os.UserHandle uh = dpm.createAndManageUser(admin, name, admin, null,
                    android.app.admin.DevicePolicyManager.MAKE_USER_EPHEMERAL
                    | android.app.admin.DevicePolicyManager.SKIP_SETUP_WIZARD);
            if (uh == null) return -1;
            dpm.startUserInBackground(admin, uh);
            return uh.getIdentifier();
        } catch (Throwable t) {
            android.util.Log.w("DiasporeChooser", "createRoamUser", t);
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
            android.util.Log.w("DiasporeChooser", "switchToUser", t);
            return false;
        }
    }

    /** Stop a leftover ephemeral user (blind-login cleanup; full removal is the gate's job on user 0). */
    private void stopRoamUser(int uid) {
        try {
            android.app.admin.DevicePolicyManager dpm =
                    (android.app.admin.DevicePolicyManager) getSystemService(Context.DEVICE_POLICY_SERVICE);
            android.content.ComponentName admin = new android.content.ComponentName(this, AdminReceiver.class);
            dpm.stopUser(admin, android.os.UserHandle.of(uid));
        } catch (Throwable ignore) {
        }
    }

    /**
     * Make a freshly-roamed user land clean: disable LineageOS's setup wizard (it ignores DPM's
     * SKIP_SETUP_WIZARD and would otherwise greet the user with "Welcome to Diaspore"), and force-stop the
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
        catch (Throwable t) { android.util.Log.w("DiasporeChooser", "ctx-as-user", t); return; }
        for (String p : ROAM_CRASHERS) {
            try { pm.setApplicationEnabledSetting(p, state, 0); }
            catch (Throwable t) { android.util.Log.w("DiasporeChooser", "setEnabled " + p, t); }
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
            android.util.Log.w("DiasporeChooser", "disable setupwizard", t);
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
