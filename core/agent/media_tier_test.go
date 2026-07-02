package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// #84 P1: classifyDeferred is the SPLIT policy -- big bulk media in known media subtrees defers; everything
// else (small, unknown-location, or login/state-critical) stays essential. A misclassification must only ever
// make login slower, never break it, so the "unknown -> essential" and deny-list behaviors are the crux.
func TestClassifyDeferred(t *testing.T) {
	big := int64(32 << 20)  // comfortably over the 8 MiB floor
	huge := int64(512 << 20)
	small := int64(4 << 20) // under the floor

	cases := []struct {
		rel  string
		size int64
		want bool
		why  string
	}{
		// the worked example: OrganicMaps offline maps -> deferred
		{"app.organicmaps/files/260527/China_Hebei.mwm", huge, true, "big map in app files/"},
		{"com.example/files/models/llm.bin", big, true, "big ML model in files/"},
		{"media/0/DCIM/Camera/VID_0001.mp4", huge, true, "big video in DCIM"},
		{"media/0/Download/movie.mkv", huge, true, "big download"},
		{"media/0/Pictures/pano.jpg", big, true, "big picture"},

		// too small -> essential regardless of location
		{"app.organicmaps/files/tiny.mwm", small, false, "under the size floor"},

		// login/state-critical -> NEVER deferred even if big + in a media dir
		{"com.example/databases/accounts.db", huge, false, "sqlite DB (deny dir)"},
		{"com.example/files/creds.keystore", huge, false, "keystore (deny ext)"},
		{"com.example/files/big.db-wal", big, false, "sqlite WAL (deny ext)"},
		{"com.example/shared_prefs/prefs_huge.xml", big, false, "shared_prefs (deny dir)"},

		// big but NOT in a known media subtree -> essential (conservative)
		{"com.example/app_webview/GPUCache/blob", huge, false, "unknown location -> essential"},
		{"com.example/lib/native.so", big, false, "code, not media"},
	}
	for _, c := range cases {
		if got := classifyDeferred(c.rel, c.size); got != c.want {
			t.Errorf("classifyDeferred(%q, %d) = %v, want %v (%s)", c.rel, c.size, got, c.want, c.why)
		}
	}
}

// The size floor is env-tunable, and 0/garbage falls back to the default.
func TestMediaMinBytes(t *testing.T) {
	t.Setenv("NOWHERE_MEDIA_MIN", "")
	if mediaMinBytes() != 8<<20 {
		t.Fatalf("default should be 8 MiB, got %d", mediaMinBytes())
	}
	t.Setenv("NOWHERE_MEDIA_MIN", "1048576") // 1 MiB
	if mediaMinBytes() != 1<<20 {
		t.Fatalf("env override failed: got %d", mediaMinBytes())
	}
	// with a 1 MiB floor, a 4 MiB map now defers
	if !classifyDeferred("app.organicmaps/files/x.mwm", 4<<20) {
		t.Fatal("a 4 MiB map should defer under a 1 MiB floor")
	}
	t.Setenv("NOWHERE_MEDIA_MIN", "bogus")
	if mediaMinBytes() != 8<<20 {
		t.Fatalf("garbage env should fall back to default, got %d", mediaMinBytes())
	}
}

// The deferred manifest field is omitempty -> a profile with no deferred media serializes byte-identical to a
// pre-#84 manifest (so #85 Sizes-style back-compat holds).
func TestManifestDeferredOmitempty(t *testing.T) {
	m := chunkManifest{Version: 2, Chunks: []string{"a", "b"}, Sizes: []int64{1, 2}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "deferred") {
		t.Fatalf("empty Deferred must be omitted from JSON, got %s", b)
	}
	// but a populated one round-trips
	m.Deferred = map[string]deferredFile{"a/files/x.mwm": {Chunks: []string{"h1", "h2"}, Size: 99, Mode: 0o644, MTime: 7}}
	b2, _ := json.Marshal(m)
	var back chunkManifest
	if err := json.Unmarshal(b2, &back); err != nil {
		t.Fatal(err)
	}
	if d, ok := back.Deferred["a/files/x.mwm"]; !ok || d.Size != 99 || len(d.Chunks) != 2 {
		t.Fatalf("deferred entry did not round-trip: %+v", back.Deferred)
	}
}
