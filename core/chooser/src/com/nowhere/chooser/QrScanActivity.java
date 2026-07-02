package com.nowhere.chooser;

import android.Manifest;
import android.app.Activity;
import android.content.Context;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.graphics.Color;
import android.graphics.SurfaceTexture;
import android.hardware.camera2.CameraAccessException;
import android.hardware.camera2.CameraCaptureSession;
import android.hardware.camera2.CameraCharacteristics;
import android.hardware.camera2.CameraDevice;
import android.hardware.camera2.CameraManager;
import android.hardware.camera2.CaptureRequest;
import android.hardware.camera2.params.StreamConfigurationMap;
import android.media.Image;
import android.media.ImageReader;
import android.os.Bundle;
import android.os.Handler;
import android.os.HandlerThread;
import android.util.Size;
import android.util.TypedValue;
import android.view.Gravity;
import android.view.Surface;
import android.view.TextureView;
import android.view.ViewGroup;
import android.widget.FrameLayout;
import android.widget.TextView;

import com.google.zxing.BarcodeFormat;
import com.google.zxing.BinaryBitmap;
import com.google.zxing.DecodeHintType;
import com.google.zxing.PlanarYUVLuminanceSource;
import com.google.zxing.Result;
import com.google.zxing.common.HybridBinarizer;
import com.google.zxing.qrcode.QRCodeReader;

import java.nio.ByteBuffer;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.EnumMap;
import java.util.List;
import java.util.Map;

/** Camera2 + ZXing QR scanner. No GApps -> ZXing core decodes offline. Returns the decoded text via the
 *  activity result extra "code" (RESULT_OK), or RESULT_CANCELED on back / denied camera. Used by the
 *  Add-credits flow so a claim code / subscription key can be scanned instead of typed (DIA-20260630-16). */
public class QrScanActivity extends Activity {
    public static final String EXTRA_CODE = "code";
    private static final int REQ_CAM = 4242;

    private TextureView preview;
    private CameraManager cm;
    private String camId;
    private CameraDevice camera;
    private CameraCaptureSession session;
    private ImageReader reader;
    private HandlerThread bgThread;
    private Handler bg;
    private Size scanSize = new Size(1280, 720);
    private final QRCodeReader qr = new QRCodeReader();
    private final Map<DecodeHintType, Object> hints = new EnumMap<>(DecodeHintType.class);
    private volatile boolean finished = false;

    private int dp(int v) {
        return (int) TypedValue.applyDimension(TypedValue.COMPLEX_UNIT_DIP, v, getResources().getDisplayMetrics());
    }

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        hints.put(DecodeHintType.POSSIBLE_FORMATS, new ArrayList<>(Arrays.asList(BarcodeFormat.QR_CODE)));
        hints.put(DecodeHintType.TRY_HARDER, Boolean.TRUE);

        FrameLayout root = new FrameLayout(this);
        root.setBackgroundColor(0xFF0B0F14);
        preview = new TextureView(this);
        root.addView(preview, new FrameLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT,
                ViewGroup.LayoutParams.MATCH_PARENT));

        TextView hint = new TextView(this);
        hint.setText("Point the camera at the QR code");
        hint.setTextColor(Color.WHITE);
        hint.setTextSize(TypedValue.COMPLEX_UNIT_SP, 16);
        hint.setShadowLayer(8, 0, 0, 0xCC000000);
        FrameLayout.LayoutParams hl = new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        hl.gravity = Gravity.BOTTOM | Gravity.CENTER_HORIZONTAL;
        hl.bottomMargin = dp(64);
        root.addView(hint, hl);
        setContentView(root);

        if (checkSelfPermission(Manifest.permission.CAMERA) != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(new String[]{Manifest.permission.CAMERA}, REQ_CAM);
        }
    }

    @Override
    protected void onResume() {
        super.onResume();
        ensureBg();
        if (checkSelfPermission(Manifest.permission.CAMERA) == PackageManager.PERMISSION_GRANTED) {
            startWhenReady();
        }
    }

    /** First-run open reliability (DIA-20260630-42): after the permission grant, onRequestPermissionsResult can
     *  reach openCamera on the main thread while onPause has already nulled the bg handler and onResume hasn't
     *  recreated it -- cm.openCamera(.., null) then throws and the scanner silently cancels (so it only worked on
     *  the 2nd open). Guarantee a live background handler wherever the open is driven from. */
    private void ensureBg() {
        if (bgThread == null) {
            bgThread = new HandlerThread("qrscan");
            bgThread.start();
            bg = new Handler(bgThread.getLooper());
        }
    }

    @Override
    public void onRequestPermissionsResult(int req, String[] perms, int[] grants) {
        if (req == REQ_CAM) {
            if (grants.length > 0 && grants[0] == PackageManager.PERMISSION_GRANTED) {
                startWhenReady();
            } else {
                cancel(); // no camera -> the user can still type the code
            }
        }
    }

    private void startWhenReady() {
        if (preview.isAvailable()) {
            openCamera();
        } else {
            preview.setSurfaceTextureListener(new TextureView.SurfaceTextureListener() {
                public void onSurfaceTextureAvailable(SurfaceTexture s, int w, int h) { openCamera(); }
                public void onSurfaceTextureSizeChanged(SurfaceTexture s, int w, int h) {}
                public boolean onSurfaceTextureDestroyed(SurfaceTexture s) { return true; }
                public void onSurfaceTextureUpdated(SurfaceTexture s) {}
            });
        }
    }

    private void openCamera() {
        if (camera != null) return; // idempotent: onResume + the permission callback can both drive this
        ensureBg();                 // a valid bg handler even if the permission callback beat onResume's recreate
        try {
            cm = (CameraManager) getSystemService(Context.CAMERA_SERVICE);
            camId = backCameraId(cm);
            if (camId == null) { cancel(); return; }
            CameraCharacteristics ch = cm.getCameraCharacteristics(camId);
            StreamConfigurationMap map = ch.get(CameraCharacteristics.SCALER_STREAM_CONFIGURATION_MAP);
            if (map != null) {
                Size best = pickSize(map.getOutputSizes(android.graphics.ImageFormat.YUV_420_888));
                if (best != null) scanSize = best;
            }
            reader = ImageReader.newInstance(scanSize.getWidth(), scanSize.getHeight(),
                    android.graphics.ImageFormat.YUV_420_888, 2);
            reader.setOnImageAvailableListener(onFrame, bg);
            if (checkSelfPermission(Manifest.permission.CAMERA) != PackageManager.PERMISSION_GRANTED) return;
            cm.openCamera(camId, stateCb, bg);
        } catch (Throwable t) {
            android.util.Log.w("QrScan", "openCamera", t);
            cancel();
        }
    }

    private final CameraDevice.StateCallback stateCb = new CameraDevice.StateCallback() {
        @Override public void onOpened(CameraDevice c) {
            camera = c;
            try {
                SurfaceTexture st = preview.getSurfaceTexture();
                st.setDefaultBufferSize(scanSize.getWidth(), scanSize.getHeight());
                Surface pv = new Surface(st);
                Surface rs = reader.getSurface();
                final CaptureRequest.Builder req = c.createCaptureRequest(CameraDevice.TEMPLATE_PREVIEW);
                req.addTarget(pv);
                req.addTarget(rs);
                req.set(CaptureRequest.CONTROL_AF_MODE, CaptureRequest.CONTROL_AF_MODE_CONTINUOUS_PICTURE);
                c.createCaptureSession(Arrays.asList(pv, rs), new CameraCaptureSession.StateCallback() {
                    @Override public void onConfigured(CameraCaptureSession s) {
                        if (camera == null) return;
                        session = s;
                        try { s.setRepeatingRequest(req.build(), null, bg); }
                        catch (Throwable t) { android.util.Log.w("QrScan", "repeating", t); }
                    }
                    @Override public void onConfigureFailed(CameraCaptureSession s) { cancel(); }
                }, bg);
            } catch (Throwable t) {
                android.util.Log.w("QrScan", "onOpened", t);
                cancel();
            }
        }
        @Override public void onDisconnected(CameraDevice c) { closeCamera(); }
        @Override public void onError(CameraDevice c, int e) { closeCamera(); cancel(); }
    };

    private final ImageReader.OnImageAvailableListener onFrame = r -> {
        Image img = null;
        try {
            img = r.acquireLatestImage();
            if (img == null || finished) return;
            Result res = decode(img);
            if (res != null && res.getText() != null && !res.getText().isEmpty()) {
                succeed(res.getText().trim());
            }
        } catch (Throwable ignore) {
            // not-found / partial frame -> just wait for the next one
        } finally {
            if (img != null) img.close();
        }
    };

    /** Decode the image's Y (luminance) plane with ZXing -- enough for QR; chroma isn't needed. */
    private Result decode(Image img) {
        Image.Plane yPlane = img.getPlanes()[0];
        ByteBuffer buf = yPlane.getBuffer();
        int w = img.getWidth(), h = img.getHeight();
        int rowStride = yPlane.getRowStride();
        byte[] y = new byte[w * h];
        if (rowStride == w) {
            buf.get(y, 0, Math.min(buf.remaining(), y.length));
        } else { // strip row padding
            byte[] row = new byte[rowStride];
            for (int r = 0; r < h; r++) {
                if (buf.remaining() < rowStride) break;
                buf.get(row, 0, rowStride);
                System.arraycopy(row, 0, y, r * w, w);
            }
        }
        PlanarYUVLuminanceSource src = new PlanarYUVLuminanceSource(y, w, h, 0, 0, w, h, false);
        BinaryBitmap bmp = new BinaryBitmap(new HybridBinarizer(src));
        try {
            return qr.decode(bmp, hints);
        } catch (Throwable t) {
            qr.reset();
            try { // a second pass on the inverted image catches light-on-dark codes
                return qr.decode(new BinaryBitmap(new HybridBinarizer(src.invert())), hints);
            } catch (Throwable t2) {
                qr.reset();
                return null;
            }
        }
    }

    private void succeed(final String code) {
        if (finished) return;
        finished = true;
        runOnUiThread(() -> {
            Intent out = new Intent();
            out.putExtra(EXTRA_CODE, code);
            setResult(RESULT_OK, out);
            finish();
        });
    }

    private void cancel() {
        if (finished) return;
        finished = true;
        runOnUiThread(() -> { setResult(RESULT_CANCELED); finish(); });
    }

    private static String backCameraId(CameraManager cm) throws CameraAccessException {
        String first = null;
        for (String id : cm.getCameraIdList()) {
            if (first == null) first = id;
            Integer f = cm.getCameraCharacteristics(id).get(CameraCharacteristics.LENS_FACING);
            if (f != null && f == CameraCharacteristics.LENS_FACING_BACK) return id;
        }
        return first; // fall back to whatever exists (e.g. an emulator's front cam)
    }

    /** Prefer a ~720p YUV size: big enough to read a QR, small enough to decode fast. */
    private static Size pickSize(Size[] sizes) {
        if (sizes == null || sizes.length == 0) return null;
        Size best = null;
        long target = 1280L * 720L;
        long bestScore = Long.MAX_VALUE;
        for (Size s : sizes) {
            if (s.getWidth() > 1920 || s.getHeight() > 1920) continue;
            long score = Math.abs((long) s.getWidth() * s.getHeight() - target);
            if (score < bestScore) { bestScore = score; best = s; }
        }
        return best;
    }

    @Override
    protected void onPause() {
        closeCamera();
        if (bgThread != null) { bgThread.quitSafely(); bgThread = null; bg = null; }
        super.onPause();
    }

    private void closeCamera() {
        try { if (session != null) { session.close(); session = null; } } catch (Throwable ignore) {}
        try { if (camera != null) { camera.close(); camera = null; } } catch (Throwable ignore) {}
        try { if (reader != null) { reader.close(); reader = null; } } catch (Throwable ignore) {}
    }
}
