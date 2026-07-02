package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func chunkHash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// The CDC chunk cache (DIA-20260618-02): content-addressed, self-healing, ciphertext-only.
func TestChunkCacheReadWrite(t *testing.T) {
	dir := t.TempDir()
	blob := []byte("sealed-chunk-ciphertext-\x00\x01\x02")
	h := chunkHash(blob)

	// An empty dir disables the cache (always a miss).
	if _, ok := cacheRead("", h); ok {
		t.Fatal("empty dir must disable the cache")
	}
	// Miss on an empty cache.
	if _, ok := cacheRead(dir, h); ok {
		t.Fatal("expected a miss on an empty cache")
	}
	// Write, then a byte-identical hit.
	cacheWrite(dir, h, blob)
	got, ok := cacheRead(dir, h)
	if !ok || string(got) != string(blob) {
		t.Fatalf("expected a hit with identical bytes (ok=%v)", ok)
	}
	// Corruption self-heals: a tampered file (content no longer hashes to h) misses AND is evicted.
	if err := os.WriteFile(filepath.Join(dir, h), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := cacheRead(dir, h); ok {
		t.Fatal("a tampered chunk must miss (hash mismatch)")
	}
	if _, err := os.Stat(filepath.Join(dir, h)); !os.IsNotExist(err) {
		t.Fatal("a corrupt chunk must be evicted on read")
	}
}

// A cache hit must short-circuit the network: getChunk returns the cached blob without ever calling
// getBlob (so the bogus base below is never dialed).
func TestGetChunkHitSkipsNetwork(t *testing.T) {
	old := chunkCacheDir
	chunkCacheDir = t.TempDir()
	defer func() { chunkCacheDir = old }()

	blob := []byte("a-cached-sealed-chunk")
	h := chunkHash(blob)
	cacheWrite(chunkCacheDir, h, blob)

	if got := getChunk("http://127.0.0.1:0/would-fail-if-dialed", h); string(got) != string(blob) {
		t.Fatal("getChunk should return the cached blob without a network fetch")
	}
}

// getChunk VERIFIES the store fetch against the content hash (#72): a TRANSIENT corruption self-heals via a
// re-fetch (the login still succeeds), while a PERSISTENT one fails as an explicit integrity panic -- never a
// cryptic auth error downstream, and never cached.
func TestGetChunkVerifiesStoreFetch(t *testing.T) {
	old := chunkCacheDir
	chunkCacheDir = "" // exercise the store path, not the local cache
	defer func() { chunkCacheDir = old }()
	rb := s3RetryBase
	s3RetryBase = time.Millisecond // shrink the backoff so the test is fast
	defer func() { s3RetryBase = rb }()

	good := bytes.Repeat([]byte("sealed"), 512)
	h := chunkHash(good)

	// Transient: the first two store reads are corrupt, the third is good -> getChunk retries and returns good.
	var hits int
	var mu sync.Mutex
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		if n <= 2 {
			w.Write([]byte("corrupt-bytes")) // hashes to something != h
			return
		}
		w.Write(good)
	}))
	defer flaky.Close()
	if got := getChunk(flaky.URL, h); !bytes.Equal(got, good) {
		t.Fatalf("getChunk should self-heal a transient corruption; got %d bytes, want %d", len(got), len(good))
	}

	// Persistent: every read is corrupt -> getChunk exhausts its retries and panics (an explicit integrity
	// error, caught by the restore's recover in prod), rather than returning unverified bytes.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("always-wrong"))
	}))
	defer bad.Close()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("a persistently corrupt store blob must panic, not return unverified bytes")
			}
		}()
		getChunk(bad.URL, h)
	}()
}
