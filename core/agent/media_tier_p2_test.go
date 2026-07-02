package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// #84 P2: with NOWHERE_ONDEMAND_MEDIA=1 a large media file is chunked into the manifest's Deferred index and
// EXCLUDED from the essential (login) restore -- yet its chunks still live in the store and reconstruct the
// original file byte-for-byte (the media daemon serves it on-access in P3). Off by default the manifest is
// unchanged (TestManifestDeferredOmitempty + the seal round-trip tests cover that).
func TestSealDefersMediaOutOfEssential(t *testing.T) {
	t.Setenv("NOWHERE_ONDEMAND_MEDIA", "1")
	t.Setenv("NOWHERE_MEDIA_MIN", "4096")           // 4 KiB floor so the test map qualifies
	t.Setenv("NOWHERE_SEAL_FILE_THRESHOLD", "4096") // and it's a "large" (file-aligned) file

	base := newHTTPStore(t)
	name, pass := "media", "on demand horse battery"
	createVault(base, name, pass)

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "app.foo/files/maps"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(src, "app.foo/shared_prefs"), 0o755)
	// essential: a small settings file (grouped, restored at login)
	if err := os.WriteFile(filepath.Join(src, "app.foo/shared_prefs/s.xml"), []byte("essential-setting"), 0o644); err != nil {
		t.Fatal(err)
	}
	// deferred: a big "map" under files/ (a media dir, over the floor)
	mapBytes := bytes.Repeat([]byte("MAPDATA-0123456789-"), 5000) // ~95 KiB
	rel := "app.foo/files/maps/region.mwm"
	if err := os.WriteFile(filepath.Join(src, rel), mapBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	pushProfile(base, name, pass, src)

	dk := resolveDK(base, name, pass)
	v, isV := parseVault(getBlob(base, getRef(base, headKey(base, name, pass))))
	if !isV {
		t.Fatal("expected a vault head")
	}
	m := loadCDCManifest(base, dk, v.Manifest)

	// (1) the map is in the Deferred index (size/chunks), not the essential Chunks
	d, ok := m.Deferred[rel]
	if !ok {
		t.Fatalf("map should be in Deferred; Deferred keys = %v", keysOf(m.Deferred))
	}
	if d.Size != int64(len(mapBytes)) || len(d.Chunks) == 0 {
		t.Fatalf("deferred entry wrong: size=%d (want %d) chunks=%d", d.Size, len(mapBytes), len(d.Chunks))
	}

	// (2) essential login restore has the settings file + the parent dir, but NOT the map
	_, dst := restoreManifest(t, base, name, pass)
	if _, err := os.Stat(filepath.Join(dst, "app.foo/shared_prefs/s.xml")); err != nil {
		t.Fatal("essential settings file missing after login restore")
	}
	if fi, err := os.Stat(filepath.Join(dst, "app.foo/files/maps")); err != nil || !fi.IsDir() {
		t.Fatal("the deferred file's parent dir must be restored at login (so the daemon can serve into it)")
	}
	if _, err := os.Stat(filepath.Join(dst, rel)); err == nil {
		t.Fatal("deferred map must NOT be restored at login -- it's served on-access")
	}

	// (3) the deferred chunks reconstruct the original file byte-for-byte
	var got []byte
	for _, h := range d.Chunks {
		raw, err := unpackChunk(unseal(dk, getChunk(base, h)), m.Version)
		if err != nil {
			t.Fatalf("unpack deferred chunk: %v", err)
		}
		got = append(got, raw...)
	}
	if !bytes.Equal(got, mapBytes) {
		t.Fatalf("deferred reconstruction mismatch: got %d bytes, want %d", len(got), len(mapBytes))
	}
}

func keysOf(m map[string]deferredFile) []string {
	k := make([]string, 0, len(m))
	for s := range m {
		k = append(k, s)
	}
	return k
}
