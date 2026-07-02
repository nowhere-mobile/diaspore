package main

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// TestContentCacheNoChangeSeal (DIA-20260626-01): a no-change re-seal must reuse every chunk's sealed form
// via the content cache (plaintext-chunk-hash -> sealed-blob-hash) -- skipping packChunk+seal+postBlob -- AND
// the resulting head must still restore byte-identically. The integrity half is the point: a wrong skip
// (reusing a sealed hash whose blob isn't actually present, or mismapping plaintext) = silent data loss.
func TestContentCacheNoChangeSeal(t *testing.T) {
	// Hermetic: fresh in-memory caches + a real on-disk state dir so the file persistence path is exercised too.
	blobCache = nil
	contentCache = nil
	contentCacheLoadedTag = ""
	atomic.StoreInt64(&contentCacheHits, 0)
	atomic.StoreInt64(&blobCacheHits, 0)
	t.Setenv("NOWHERE_BLOBCACHE", t.TempDir())

	base := newHTTPStore(t)
	name, pass := "ccache", "content cache horse battery staple"
	createVault(base, name, pass)

	// ~6.6 MB over two files -> the CDC produces several chunks, so the ordered reassembly + multi-chunk reuse
	// are genuinely exercised.
	src := t.TempDir()
	big := bytes.Repeat([]byte("content-cache round trip 0123456789 abcdefghij\n"), 140000)
	if err := os.WriteFile(filepath.Join(src, "big.txt"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	small := []byte("a small file restored after the big one")
	if err := os.WriteFile(filepath.Join(src, "small.txt"), small, 0o600); err != nil {
		t.Fatal(err)
	}

	// 1) First push: nothing cached yet -> all misses, but it RECORDS each chunk's plaintext->sealed mapping.
	pushProfile(base, name, pass, src)
	if h := atomic.LoadInt64(&contentCacheHits); h != 0 {
		t.Fatalf("first push must be all misses, got %d content-cache hits", h)
	}

	// 2) Restore: seeds BOTH caches from the decrypted chunks (blob = confirmed present, content = plaintext->sealed)
	//    -- this is the warm-from-login path. Restore must be byte-identical.
	m, dst := restoreManifest(t, base, name, pass)
	if len(m.Chunks) < 2 {
		t.Fatalf("expected multiple chunks to exercise reuse, got %d", len(m.Chunks))
	}
	if got, err := os.ReadFile(filepath.Join(dst, "big.txt")); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, big) {
		t.Fatal("restore #1 big.txt mismatch")
	}

	// 3) Second push, NO source change. Simulate the on-device process boundary: the logoff seal runs in a FRESH
	//    roamd process, so drop BOTH in-memory maps -> the hit must come from the on-disk $STATE files (proving the
	//    file format round-trips). Every chunk's plaintext hash hits the content cache AND blobCacheKnown confirms
	//    the sealed blob is present -> skip packChunk+seal+postBlob for every chunk.
	blobCache = nil
	contentCache = nil
	contentCacheLoadedTag = ""
	atomic.StoreInt64(&contentCacheHits, 0)
	pushProfile(base, name, pass, src)
	if hits := atomic.LoadInt64(&contentCacheHits); int(hits) < len(m.Chunks) {
		t.Fatalf("no-change re-seal must hit the content cache for every chunk: got %d, want >= %d", hits, len(m.Chunks))
	}

	// 4) CRITICAL: the cache-hit seal must not have corrupted the head -- restore again and byte-compare both files.
	_, dst2 := restoreManifest(t, base, name, pass)
	if got, err := os.ReadFile(filepath.Join(dst2, "big.txt")); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, big) {
		t.Fatalf("post-cache-hit restore mismatch: %d B vs %d B (a wrong skip corrupted the head)", len(got), len(big))
	}
	if got, err := os.ReadFile(filepath.Join(dst2, "small.txt")); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, small) {
		t.Fatal("post-cache-hit small.txt mismatch")
	}
}

// TestContentCacheSafetyMissingBlob (DIA-20260626-01): the content cache must NEVER skip a seal when the
// sealed blob isn't confirmed present (blobCacheKnown=false). Even with a populated plaintext->sealed mapping,
// an unconfirmed blob must force the full seal+upload, so a head can never point at a GC'd/absent blob.
func TestContentCacheSafetyMissingBlob(t *testing.T) {
	blobCache = nil
	contentCache = nil
	contentCacheLoadedTag = ""
	atomic.StoreInt64(&contentCacheHits, 0)
	atomic.StoreInt64(&blobCacheHits, 0)
	t.Setenv("NOWHERE_BLOBCACHE", t.TempDir())

	base := newHTTPStore(t)
	name, pass := "ccsafety", "content cache safety horse battery"
	createVault(base, name, pass)

	// Random -> many DISTINCT chunks (no dedup collapse), so "every chunk re-uploaded" is a meaningful check.
	src := t.TempDir()
	data := make([]byte, 6*1024*1024)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "d.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	pushProfile(base, name, pass, src)           // warms the content cache (plaintext->sealed, in memory)
	m, _ := restoreManifest(t, base, name, pass) // learn the chunk set
	if len(m.Chunks) < 2 {
		t.Fatalf("want multiple distinct chunks, got %d", len(m.Chunks))
	}

	// Force "blob no longer confirmed present": an EMPTY but non-nil map -> blobCacheInit won't reload from the
	// file, so blobCacheKnown=false for every chunk. The plaintext->sealed mapping still resolves, but the safety
	// gate must REJECT every reuse and re-seal -- otherwise a head could point at a GC'd blob (silent data loss).
	blobCache = map[string]bool{}
	atomic.StoreInt64(&contentCacheHits, 0)
	pushProfile(base, name, pass, src)

	// contentCacheHits counts ACTUAL skips: under an unknown blob cache it must be ZERO (the gate rejected every
	// mapping and re-sealed), even though the content map was fully populated.
	if h := atomic.LoadInt64(&contentCacheHits); h != 0 {
		t.Fatalf("safety: the gate skipped %d chunk(s) despite blobCacheKnown=false -- it trusted a stale mapping (data loss)", h)
	}
	// And the head still restores byte-identically after the forced re-seal.
	_, dst := restoreManifest(t, base, name, pass)
	if got, err := os.ReadFile(filepath.Join(dst, "d.bin")); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, data) {
		t.Fatal("safety: restore mismatch after the forced re-seal")
	}
}
