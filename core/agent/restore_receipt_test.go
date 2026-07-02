package main

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

// #72 keystone: a session that did NOT fully restore a ref must not seal its (partial) working set over that
// ref's good head. The gate is a per-ref restore-completion RECEIPT (restore_receipt.go): a full restore or a
// successful seal writes one; pushProfile refuses to overwrite a non-empty head without it. These tests drive
// the gate directly (via NOWHERE_STATE + the receipt helpers) rather than the whole roamd phase pipeline.

// headOf returns the profile's current store head pointer (empty if none).
func headOf(base, name, pass string) string { return getRef(base, headKey(base, name, pass)) }

// TestRestoreReceiptGateBlocksPartialSession: after a good head exists, a NEW session with no receipt (a
// stand-in for a failed/partial restore) is REFUSED and the good head survives; once a receipt is present
// (a completed restore) the same seal proceeds. Chunk counts are kept < 20 so the shrink-count net never
// fires -- this isolates the RECEIPT gate, the exact gap the shrink guard misses (a moderate partial).
func TestRestoreReceiptGateBlocksPartialSession(t *testing.T) {
	t.Setenv("NOWHERE_STATE", t.TempDir()) // activates the gate + roots the receipts dir
	base := newHTTPStore(t)
	name, pass := "keystone", "receipt horse battery staple"
	createVault(base, name, pass)

	// First seal: an empty vault head (oldN==0) is exempt, so this proceeds and records a receipt.
	src1 := t.TempDir()
	big := make([]byte, 2<<20) // ~2 MiB random -> a small handful of chunks, well under the shrink-net's 20
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src1, "one.bin"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	pushProfile(base, name, pass, src1)
	head1 := headOf(base, name, pass)
	ref := profileRefV2(name, pass) // #80: the receipt key is the STABLE v2 identity ref (what pushProfile/login use)
	if head1 == "" {
		t.Fatal("first seal did not establish a head")
	}
	if !haveRestoreReceipt(ref) {
		t.Fatal("a successful seal must record a completion receipt")
	}

	// New session that did NOT complete a restore: clear the receipts (exactly what roam-in does at login).
	os.RemoveAll(restoreReceiptDir())
	if haveRestoreReceipt(ref) {
		t.Fatal("receipts not cleared")
	}

	// A moderately smaller working set (a partial restore) tries to seal. It must be REFUSED: the head is
	// unchanged and the store still restores the ORIGINAL data.
	src2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(src2, "two.txt"), []byte("partial session"), 0o600); err != nil {
		t.Fatal(err)
	}
	pushProfile(base, name, pass, src2)
	if got := headOf(base, name, pass); got != head1 {
		t.Fatalf("partial session sealed over the good head (head %s -> %s) -- #72 data loss", short(head1), short(got))
	}
	_, dst := restoreManifest(t, base, name, pass)
	if _, err := os.Stat(filepath.Join(dst, "one.bin")); err != nil {
		t.Fatal("good data lost: one.bin missing after a refused partial seal")
	}
	if _, err := os.Stat(filepath.Join(dst, "two.txt")); err == nil {
		t.Fatal("partial data leaked into the head despite the refusal")
	}

	// Now the session proves it fully holds the ref (a completed restore writes this receipt). The same seal
	// must now proceed and advance the head to the new working set.
	writeRestoreReceipt(ref, head1)
	pushProfile(base, name, pass, src2)
	if got := headOf(base, name, pass); got == head1 {
		t.Fatal("seal with a valid receipt was still refused")
	}
	_, dst2 := restoreManifest(t, base, name, pass)
	if got, err := os.ReadFile(filepath.Join(dst2, "two.txt")); err != nil || !bytes.Equal(got, []byte("partial session")) {
		t.Fatal("head did not advance to the new working set after a permitted seal")
	}
}

// TestFreshProfileFirstSealNotBlocked: with the gate ACTIVE, a newly created profile (empty head, no receipt)
// must still seal its first real working set -- an empty head has nothing to protect (oldN==0 is exempt).
func TestFreshProfileFirstSealNotBlocked(t *testing.T) {
	t.Setenv("NOWHERE_STATE", t.TempDir())
	base := newHTTPStore(t)
	name, pass := "fresh", "first seal horse battery staple"
	createVault(base, name, pass)
	empty := headOf(base, name, pass)

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("brand new profile"), 0o600); err != nil {
		t.Fatal(err)
	}
	pushProfile(base, name, pass, src) // no receipt yet, but oldN==0 -> must proceed
	if got := headOf(base, name, pass); got == empty {
		t.Fatal("a fresh profile's first seal was wrongly refused (empty head must be exempt)")
	}
	if !haveRestoreReceipt(profileRefV2(name, pass)) {
		t.Fatal("the first seal should have recorded a receipt")
	}
}

// TestReceiptGateInertOffDevice: with NO roaming state dir (host CLI / the existing test suite), the gate is
// disabled -- a seal over a non-empty head with no receipt proceeds, so pre-existing behavior is unchanged.
func TestReceiptGateInertOffDevice(t *testing.T) {
	if restoreReceiptDir() != "" {
		t.Skip("NOWHERE_STATE is set in this environment")
	}
	base := newHTTPStore(t)
	name, pass := "hostcli", "inert horse battery staple"
	createVault(base, name, pass)

	src1 := t.TempDir()
	if err := os.WriteFile(filepath.Join(src1, "a.txt"), bytes.Repeat([]byte("data\n"), 100), 0o600); err != nil {
		t.Fatal(err)
	}
	pushProfile(base, name, pass, src1)
	head1 := headOf(base, name, pass)

	src2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(src2, "b.txt"), []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	pushProfile(base, name, pass, src2) // no receipts anywhere, but the gate is off -> must proceed
	if got := headOf(base, name, pass); got == head1 {
		t.Fatal("off-device seal was blocked -- the gate must be inert without NOWHERE_STATE")
	}
}
