package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// #85: pushProfile records each chunk's sealed blob size in the manifest so profileFootprint (and billing's
// payRent/leaseRefs) size the profile from the manifest alone -- no per-chunk store round-trip (which in cap
// mode DOWNLOADED every chunk just to measure it). Assert the manifest carries a correct Size for every
// chunk, that a no-change re-seal carries those sizes FORWARD (dedup path, no re-seal), and that the
// footprint total agrees with the chunk sizes.
func TestManifestChunkSizes(t *testing.T) {
	base := newHTTPStore(t)
	name, pass := "sizes", "footprint horse battery staple"
	createVault(base, name, pass)

	src := t.TempDir()
	big := make([]byte, 6<<20) // ~6 MiB random -> several chunks
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.bin"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "b.txt"), []byte("a second file"), 0o600); err != nil {
		t.Fatal(err)
	}

	// checkManifestSizes reads the head -> manifest and verifies every chunk's recorded size == the real blob.
	checkManifestSizes := func(stage string) chunkManifest {
		key, mb, ok := resolveKey(base, name, pass)
		if !ok {
			t.Fatalf("%s: resolveKey failed", stage)
		}
		pt := unseal(key, mb)
		if !bytes.HasPrefix(pt, cdcMagic) {
			t.Fatalf("%s: head is not a CDC manifest", stage)
		}
		var m chunkManifest
		if err := json.Unmarshal(pt[len(cdcMagic):], &m); err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		if len(m.Chunks) < 2 {
			t.Fatalf("%s: expected multiple chunks to exercise this, got %d", stage, len(m.Chunks))
		}
		if len(m.Sizes) != len(m.Chunks) {
			t.Fatalf("%s: Sizes not populated: %d sizes for %d chunks", stage, len(m.Sizes), len(m.Chunks))
		}
		for i, ch := range m.Chunks {
			if want := int64(len(getBlob(base, ch))); m.Sizes[i] != want {
				t.Fatalf("%s: chunk %d recorded size %d != actual blob %d", stage, i, m.Sizes[i], want)
			}
		}
		return m
	}

	pushProfile(base, name, pass, src) // fresh seal: sizes come from the sealed length
	m1 := checkManifestSizes("first seal")

	pushProfile(base, name, pass, src) // no-change re-seal: all chunks dedup -> sizes CARRIED FORWARD, not re-sealed
	m2 := checkManifestSizes("carry-forward re-seal")

	// The carry-forward seal must reproduce the same chunk sizes (same content -> same sealed blobs).
	if len(m1.Sizes) != len(m2.Sizes) {
		t.Fatalf("chunk count changed across a no-op re-seal: %d -> %d", len(m1.Sizes), len(m2.Sizes))
	}

	// footprint total = head + manifest + sum(chunk sizes); it must at least cover the chunk bytes.
	var chunkSum int64
	for _, sz := range m2.Sizes {
		chunkSum += sz
	}
	var total int64
	for _, sz := range profileFootprint(base, name, pass) {
		total += sz
	}
	if total < chunkSum || chunkSum == 0 {
		t.Fatalf("footprint total %d does not cover chunk sum %d", total, chunkSum)
	}
}
