package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestCDCParallelRoundTrip proves the DIA-20260624-07 parallel paths preserve order: a push (parallel
// seal+upload) followed by a restore (parallel cdcRestore: concurrent fetch, strictly-ordered write) must
// reconstruct the source byte-identically, across MANY chunks. (Uses newHTTPStore from delete_test.go.)
func TestCDCParallelRoundTrip(t *testing.T) {
	base := newHTTPStore(t)
	name, pass := "frank", "round trip horse battery staple"
	createVault(base, name, pass)

	// ~10 MB of random content -> the gear-hash CDC produces many chunks (~1 MB avg), so the ordered
	// reassembly is genuinely exercised; plus a second small file that must land in order after it.
	src := t.TempDir()
	big := make([]byte, 10*1024*1024)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	small := []byte("a small second file, restored in order after the big one")
	if err := os.WriteFile(filepath.Join(src, "big.bin"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "small.txt"), small, 0o600); err != nil {
		t.Fatal(err)
	}

	pushProfile(base, name, pass, src) // parallel seal + upload

	key, manifestBlob, ok := resolveKey(base, name, pass)
	if !ok {
		t.Fatal("resolveKey failed after push")
	}
	pt := unseal(key, manifestBlob)
	if !bytes.HasPrefix(pt, cdcMagic) {
		t.Fatal("head is not a CDC manifest")
	}
	var m chunkManifest
	if err := json.Unmarshal(pt[len(cdcMagic):], &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Chunks) < 2 {
		t.Fatalf("expected multiple chunks to exercise ordering, got %d", len(m.Chunks))
	}

	dst := t.TempDir()
	cdcRestore(base, key, m, dst) // parallel fetch, ordered write

	if got, err := os.ReadFile(filepath.Join(dst, "big.bin")); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, big) {
		t.Fatalf("big.bin mismatch: restored %d B, want %d B (chunk reassembly out of order?)", len(got), len(big))
	}
	if got, err := os.ReadFile(filepath.Join(dst, "small.txt")); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, small) {
		t.Fatal("small.txt mismatch")
	}
}

// restoreManifest resolves a profile's CDC manifest then restores it to a fresh dir via cdcRestore.
func restoreManifest(t *testing.T, base, name, pass string) (chunkManifest, string) {
	t.Helper()
	key, mb, ok := resolveKey(base, name, pass)
	if !ok {
		t.Fatal("resolveKey failed")
	}
	pt := unseal(key, mb)
	if !bytes.HasPrefix(pt, cdcMagic) {
		t.Fatal("head is not a CDC manifest")
	}
	var m chunkManifest
	if err := json.Unmarshal(pt[len(cdcMagic):], &m); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	cdcRestore(base, key, m, dst)
	return m, dst
}

// TestCDCCompressionRoundTrip (DIA-20260624-08): compressible data round-trips byte-identically with
// compression ON (manifest V2 = zstd) and OFF (V1, backward-compatible), and the codec is deterministic
// (so convergent dedup is preserved).
func TestCDCCompressionRoundTrip(t *testing.T) {
	base := newHTTPStore(t)
	name, pass := "grace", "compress horse battery staple"
	createVault(base, name, pass)

	src := t.TempDir()
	data := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog 0123456789\n"), 200000) // ~10 MB, very compressible
	if err := os.WriteFile(filepath.Join(src, "log.txt"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	// Compression ON (default): V2 manifest, multi-chunk, byte-identical restore.
	pushProfile(base, name, pass, src)
	m, dst := restoreManifest(t, base, name, pass)
	if m.Version != 2 {
		t.Fatalf("compress-on: expected a V2 manifest, got V%d", m.Version)
	}
	if len(m.Chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(m.Chunks))
	}
	if got, err := os.ReadFile(filepath.Join(dst, "log.txt")); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, data) {
		t.Fatalf("compress-on round-trip mismatch: %d B vs %d B", len(got), len(data))
	}

	// Compression OFF: V1 manifest, still byte-identical (the pre-change / backward-compatible path).
	t.Setenv("NOWHERE_COMPRESS", "0")
	pushProfile(base, name, pass, src)
	m, dst = restoreManifest(t, base, name, pass)
	if m.Version != 1 {
		t.Fatalf("compress-off: expected a V1 manifest, got V%d", m.Version)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "log.txt")); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, data) {
		t.Fatal("compress-off round-trip mismatch")
	}

	// Deterministic compression -> the convergent seal still dedups (same chunk -> same bytes).
	c := []byte("repetitive repetitive repetitive content content content content")
	if !bytes.Equal(packChunk(c, true), packChunk(c, true)) {
		t.Fatal("zstd compression is not deterministic -> would break convergent dedup")
	}
}
