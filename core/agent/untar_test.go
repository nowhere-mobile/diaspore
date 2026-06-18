package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// safeJoin is the tar-slip / path-traversal guard (audit #1). untarFrom runs as ROOT via the su:s0 workers,
// so an entry that escapes the destination would be an arbitrary root file write.
func TestSafeJoin(t *testing.T) {
	dir := "/data/user/10"
	cases := []struct {
		name string
		ok   bool
	}{
		{"a/b", true},
		{"pkg/files/x.db", true},
		{".", true},
		{"../evil", false},
		{"../../etc/passwd", false},
		{"a/../../../etc", false},
		{"/abs/path", true}, // absolute name is joined UNDER dir -> stays inside
	}
	for _, c := range cases {
		if _, ok := safeJoin(dir, c.name); ok != c.ok {
			t.Errorf("safeJoin(%q, %q) ok=%v want %v", dir, c.name, ok, c.ok)
		}
	}
}

// A tar whose entries try to climb out of the destination must NOT write outside it; safe entries still do.
func TestUntarFromRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "a", "b") // two levels deep so a (rejected) climb still lands inside root
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(name, body string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("ok.txt", "good")            // -> dest/ok.txt
	add("sub/ok2.txt", "good2")      // -> dest/sub/ok2.txt
	add("../sib.txt", "evil")        // -> root/a/sib.txt (must be rejected)
	add("../../top.txt", "evil2")    // -> root/top.txt   (must be rejected)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	untarFrom(dest, bytes.NewReader(buf.Bytes()))

	// safe entries land inside dest
	if b, err := os.ReadFile(filepath.Join(dest, "ok.txt")); err != nil || string(b) != "good" {
		t.Errorf("safe entry ok.txt not extracted: %v / %q", err, string(b))
	}
	if _, err := os.Stat(filepath.Join(dest, "sub", "ok2.txt")); err != nil {
		t.Errorf("safe nested entry not extracted: %v", err)
	}
	// traversal entries must NOT have escaped dest
	for _, p := range []string{filepath.Join(root, "a", "sib.txt"), filepath.Join(root, "top.txt")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("tar-slip wrote outside dest: %s", p)
		}
	}
}
