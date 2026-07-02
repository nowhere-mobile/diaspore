package main

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// Endospore E.3b: enrolling an identity to a device's secure element HARDENS it -- the pass-only slot is
// dropped, so it opens ONLY via the SE slot (passphrase + that device's se_secret) or recovery. A captured
// store ciphertext is then not offline-brute-forceable by passphrase alone. See docs/se-binding.md.
func TestEnrollSEHardensAndUnlocks(t *testing.T) {
	base := newHTTPStore(t)
	name, pass := "alice", "correct horse battery staple"
	createVault(base, name, pass)

	dk0, _, ok := resolveKey(base, name, pass)
	if !ok {
		t.Fatal("setup: passphrase login should work before enrollment")
	}

	seSecret := make([]byte, 32)
	for i := range seSecret {
		seSecret[i] = byte(i) + 1
	}
	if err := enrollSE(base, name, pass, seSecret); err != nil {
		t.Fatalf("enrollSE: %v", err)
	}

	// Hardened: passphrase ALONE (no se_secret in the env) must no longer open the head.
	t.Setenv("NOWHERE_SE_SECRET", "")
	if _, _, ok := resolveKey(base, name, pass); ok {
		t.Fatal("hardened identity must NOT open with the passphrase alone (offline-brute-force gap)")
	}

	// The SE slot opens with the right se_secret, yielding the ORIGINAL DK (data stays decryptable).
	dk1, _, ok := resolveKeySE(base, name, pass, seSecret)
	if !ok || !bytes.Equal(dk0, dk1) {
		t.Fatal("resolveKeySE should open the hardened identity and recover the original DK")
	}

	// resolveKey transparently uses the SE slot when this device supplies NOWHERE_SE_SECRET.
	t.Setenv("NOWHERE_SE_SECRET", hex.EncodeToString(seSecret))
	dk2, _, ok := resolveKey(base, name, pass)
	if !ok || !bytes.Equal(dk0, dk2) {
		t.Fatal("resolveKey via NOWHERE_SE_SECRET should yield the original DK")
	}

	// A wrong se_secret must not open it (brute-forcing the passphrase still needs the real SE secret).
	if _, _, ok := resolveKeySE(base, name, pass, make([]byte, 32)); ok {
		t.Fatal("a wrong se_secret must not open the SE slot")
	}
}

// Roaming to a new (un-enrolled) device still works on a hardened identity via the recovery mnemonic --
// which re-establishes a pass slot there (the device can then re-enroll to its own SE).
func TestEnrollSEPreservesRecovery(t *testing.T) {
	base := newHTTPStore(t)
	name, pass := "bob", "passphrase one two three"
	mnemonic := createVault(base, name, pass)
	if err := enrollSE(base, name, pass, []byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatalf("enrollSE: %v", err)
	}
	if err := recoverVault(base, name, mnemonic, "new passphrase on a fresh device"); err != nil {
		t.Fatalf("recovery (roaming path) must still work after hardening: %v", err)
	}
}

// kekSE binds both the passphrase and se_secret: changing either yields a different KEK.
func TestKekSEBindsPassAndSecret(t *testing.T) {
	s1 := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	s2 := []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	base := kekSE("alice", "pass", s1)
	if bytes.Equal(base, kekSE("alice", "pass", s2)) {
		t.Fatal("different se_secret must give a different KEK")
	}
	if bytes.Equal(base, kekSE("alice", "other", s1)) {
		t.Fatal("different passphrase must give a different KEK")
	}
	if !bytes.Equal(base, kekSE("alice", "pass", s1)) {
		t.Fatal("kekSE must be deterministic for the same inputs")
	}
}
