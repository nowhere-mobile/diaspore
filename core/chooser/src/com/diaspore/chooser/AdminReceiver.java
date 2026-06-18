package com.diaspore.chooser;

import android.app.admin.DeviceAdminReceiver;

/**
 * Minimal device-admin receiver. Its only purpose is to let the chooser become the DEVICE OWNER, which
 * is what allows it to drive Lock Task Mode (kiosk) silently -- so the gate can disable the gesture-nav
 * overview/home and the notification shade, with no way to "minimize" to the app switcher.
 */
public class AdminReceiver extends DeviceAdminReceiver {
}
