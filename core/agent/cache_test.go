package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
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
