package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// #80 P2a: the v2 ref is an HMAC over the Argon2 key -> depends on name+pass, deterministic, and distinct from
// the legacy sha256 ref (which it replaces to kill the fast existence oracle).
func TestProfileRefV2(t *testing.T) {
	a := profileRefV2("alice", "pw1")
	if len(a) != 64 {
		t.Fatalf("v2 ref should be 64 hex chars, got %q", a)
	}
	if a == profileRefV2("alice", "pw2") {
		t.Fatal("v2 ref must depend on the passphrase")
	}
	if a == profileRefV2("bob", "pw1") {
		t.Fatal("v2 ref must depend on the name")
	}
	if a != profileRefV2("alice", "pw1") {
		t.Fatal("v2 ref must be deterministic")
	}
	if a == profileRefLegacy("alice", "pw1") {
		t.Fatal("v2 ref must differ from the legacy sha256 ref")
	}
}

// deriveKey memoization: the many ref/KEK/DK derivations for ONE identity must cost a single Argon2.
func TestDeriveKeyMemoized(t *testing.T) {
	deriveKeyMu.Lock()
	deriveKeyCache = map[string][]byte{}
	deriveKeyMisses = 0
	deriveKeyMu.Unlock()

	for i := 0; i < 25; i++ { // ref + key derivations for the same (name,pass)
		_ = deriveKey("carol", "battery horse staple")
		_ = profileRefV2("carol", "battery horse staple")
	}
	deriveKeyMu.Lock()
	misses := deriveKeyMisses
	deriveKeyMu.Unlock()
	if misses != 1 {
		t.Fatalf("Argon2 must run ONCE per (name,pass): got %d", misses)
	}

	_ = deriveKey("dave", "another pass") // a different identity -> exactly one more Argon2
	deriveKeyMu.Lock()
	misses = deriveKeyMisses
	deriveKeyMu.Unlock()
	if misses != 2 {
		t.Fatalf("a new identity must add exactly one Argon2: got %d", misses)
	}
}

// headKey resolves to v2 when present, else the legacy ref, else v2 (new profile); putHead migrates a legacy
// head to v2 and tombstones the legacy ref.
func TestHeadKeyAndPutHeadMigration(t *testing.T) {
	base := newHTTPStore(t)
	name, pass := "eve", "correct horse battery"
	v2, lg := profileRefV2(name, pass), profileRefLegacy(name, pass)

	// nothing exists -> a brand-new profile resolves to v2, and it's blank
	if got := headKey(base, name, pass); got != v2 {
		t.Fatalf("headKey with no head must be v2, got %s", got)
	}
	if refExists(base, v2) || refExists(base, lg) {
		t.Fatal("no head should exist yet")
	}

	// legacy-only head (an un-migrated profile) -> headKey falls back to it
	putRef(base, lg, "legacyhead")
	if got := headKey(base, name, pass); got != lg {
		t.Fatalf("headKey must fall back to the legacy ref, got %s", got)
	}

	// putHead migrates: writes the v2 head + tombstones legacy; headKey now prefers v2
	putHead(base, name, pass, "v2head")
	if getRef(base, v2) != "v2head" {
		t.Fatal("putHead must write the head at v2")
	}
	if getRef(base, lg) != "" {
		t.Fatal("putHead must tombstone the legacy ref (empty)")
	}
	if got := headKey(base, name, pass); got != v2 {
		t.Fatalf("after migration headKey must resolve to v2, got %s", got)
	}
}

// #80 P2e end-to-end matrix: a REAL vault lifecycle through the actual ops (createVault/pushProfile/
// deleteProfile), proving (1) a fresh profile is born at v2 with NO legacy ref (no oracle surface), (2) a
// pre-#80 profile whose head lives ONLY at the legacy sha256 ref logs in via the fallback and MIGRATES to v2
// (tombstoning the legacy ref) on its first seal with data preserved, (3) a wrong passphrase resolves nothing
// (blind), and (4) delete tombstones BOTH ref schemes. A missed routing site would surface here as a blank
// login or an un-migrated head.
func TestKDFRefMigrationLifecycle(t *testing.T) {
	base := newHTTPStore(t)
	name, pass := "migrator", "correct horse battery staple"
	v2, lg := profileRefV2(name, pass), profileRefLegacy(name, pass)

	// (1) fresh profile: born at v2, no legacy ref ever created
	createVault(base, name, pass)
	if !refExists(base, v2) {
		t.Fatal("createVault must write the head at the v2 ref")
	}
	if refExists(base, lg) {
		t.Fatal("a fresh profile must NOT create a legacy ref (no fast-oracle surface)")
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "note.txt"), []byte("original data"), 0o600); err != nil {
		t.Fatal(err)
	}
	pushProfile(base, name, pass, src)
	hv := getRef(base, v2)
	if hv == "" {
		t.Fatal("seal did not establish a v2 head")
	}

	// (2) model a PRE-#80 profile: plant the head at the legacy ref and make v2 ABSENT (hard-remove, so it
	// reads not-found rather than an empty tombstone -- headKey must then fall back to legacy).
	req, _ := http.NewRequest(http.MethodDelete, base+"/ref/"+v2, nil)
	if resp, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	putRef(base, lg, hv)
	if refExists(base, v2) {
		t.Fatal("setup: v2 must read absent to model an un-migrated profile")
	}
	if got := headKey(base, name, pass); got != lg {
		t.Fatalf("headKey must fall back to the legacy ref for an un-migrated profile, got %s", got)
	}
	_, dst := restoreManifest(t, base, name, pass) // login via the legacy fallback + data intact
	if got, _ := os.ReadFile(filepath.Join(dst, "note.txt")); string(got) != "original data" {
		t.Fatal("un-migrated (legacy-ref) profile did not restore its data")
	}

	// (2b) the first seal MIGRATES the head to v2 + tombstones legacy, data preserved
	if err := os.WriteFile(filepath.Join(src, "note.txt"), []byte("post-migration data"), 0o600); err != nil {
		t.Fatal(err)
	}
	pushProfile(base, name, pass, src)
	if getRef(base, v2) == "" {
		t.Fatal("the first seal must migrate the head to the v2 ref")
	}
	if getRef(base, lg) != "" {
		t.Fatal("migration must tombstone the legacy ref (reads empty)")
	}
	if got := headKey(base, name, pass); got != v2 {
		t.Fatalf("after migration headKey must resolve to v2, got %s", got)
	}
	_, dst2 := restoreManifest(t, base, name, pass)
	if got, _ := os.ReadFile(filepath.Join(dst2, "note.txt")); string(got) != "post-migration data" {
		t.Fatal("migrated head did not carry the new working set")
	}

	// (3) a wrong passphrase resolves nothing (blind login preserved)
	if h := getRef(base, headKey(base, name, "wrong-pass")); h != "" {
		t.Fatalf("a wrong passphrase must resolve nothing, got %q", h)
	}

	// (4) delete tombstones BOTH ref schemes -> neither resolves
	if !deleteProfile(base, name, pass) {
		t.Fatal("delete of a migrated profile should succeed")
	}
	if getRef(base, v2) != "" {
		t.Fatal("delete must tombstone the v2 head")
	}
	if getRef(base, lg) != "" {
		t.Fatal("delete must tombstone the legacy head")
	}
}

// #80: rotating the passphrase publishes the new-pass head at ITS v2 ref and invalidates the old passphrase at
// BOTH schemes (a lingering legacy old-pass ref would keep the sha256 oracle alive).
func TestRotateVaultLandsAtV2(t *testing.T) {
	base := newHTTPStore(t)
	name, oldpass, newpass := "rotator", "old horse battery", "new horse battery staple"
	createVault(base, name, oldpass)
	if err := rotateVault(base, name, oldpass, newpass); err != nil {
		t.Fatal(err)
	}
	if getRef(base, profileRefV2(name, newpass)) == "" {
		t.Fatal("rotate must publish the new-pass head at its v2 ref")
	}
	if getRef(base, profileRefV2(name, oldpass)) != "" {
		t.Fatal("rotate must invalidate the old-pass v2 ref")
	}
	if getRef(base, profileRefLegacy(name, oldpass)) != "" {
		t.Fatal("rotate must invalidate the old-pass legacy ref")
	}
	if headOf(base, name, newpass) == "" {
		t.Fatal("the new passphrase must resolve after a rotate")
	}
	if headOf(base, name, oldpass) != "" {
		t.Fatal("the old passphrase must NOT resolve after a rotate")
	}
}

// #80: recovery via the 12-word code resets the passphrase and lands the new-pass head at its v2 ref (the
// recovery ref itself stays sha256 -- it's reached only with the high-entropy mnemonic, not a guessable pass).
func TestRecoverVaultLandsAtV2(t *testing.T) {
	base := newHTTPStore(t)
	name, pass, newpass := "recoverer", "forgotten pass", "fresh pass horse battery"
	mnemonic := createVault(base, name, pass)
	if mnemonic == "" {
		t.Fatal("createVault must return a recovery mnemonic")
	}
	if err := recoverVault(base, name, mnemonic, newpass); err != nil {
		t.Fatal(err)
	}
	if getRef(base, profileRefV2(name, newpass)) == "" {
		t.Fatal("recover must publish the new-pass head at its v2 ref")
	}
	if headOf(base, name, newpass) == "" {
		t.Fatal("the recovered passphrase must resolve")
	}
}

// #80 + #72: with the restore-receipt gate ACTIVE, a session that migrates a legacy head to v2 mid-session must
// keep sealing AFTER the migration. The receipt is keyed by the STABLE profileRefV2, not headKey -- if it were
// keyed by headKey (which flips legacy->v2 on the migrating seal), the login-time receipt would be orphaned and
// EVERY seal after the first (incl. the logoff seal) would be refused, silently dropping session-end data. This
// reproduces the exact on-device flow (managed profile "c" migrating on its first seal). The lifecycle test above
// runs with the gate INERT (no NOWHERE_STATE), which is why it does not catch this.
func TestReceiptSurvivesMigration(t *testing.T) {
	t.Setenv("NOWHERE_STATE", t.TempDir()) // activate the #72 receipt gate
	base := newHTTPStore(t)
	name, pass := "migreceipt", "correct horse battery staple"
	v2, lg := profileRefV2(name, pass), profileRefLegacy(name, pass)

	// seed a profile with real data, then relocate its head to the LEGACY ref -> a pre-#80 profile
	createVault(base, name, pass)
	src := t.TempDir()
	body := bytes.Repeat([]byte("receipt-migration-payload "), 150000) // ~4 MB -> a handful of chunks (<20)
	if err := os.WriteFile(filepath.Join(src, "f.bin"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	pushProfile(base, name, pass, src) // v2-native head carrying data
	hv := getRef(base, v2)
	req, _ := http.NewRequest(http.MethodDelete, base+"/ref/"+v2, nil)
	if resp, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	putRef(base, lg, hv) // head now lives ONLY at the legacy ref
	if headKey(base, name, pass) != lg {
		t.Fatal("setup: head should resolve to the legacy ref")
	}

	// model a fresh login: clear receipts, then write the roam-in completion receipt exactly as login does
	os.RemoveAll(restoreReceiptDir())
	writeRestoreReceipt(profileRefV2(name, pass), hv)

	// seal #1 MIGRATES legacy->v2 (receipt present -> permitted); head advances to v2, legacy tombstoned
	if err := os.WriteFile(filepath.Join(src, "f.bin"), append(body, 'x'), 0o600); err != nil {
		t.Fatal(err)
	}
	pushProfile(base, name, pass, src)
	h1 := getRef(base, v2)
	if h1 == "" {
		t.Fatal("seal #1 (migrating) was refused or failed to migrate to v2")
	}
	if getRef(base, lg) != "" {
		t.Fatal("seal #1 must tombstone the legacy ref")
	}

	// seal #2 runs AFTER migration (headKey now == v2). It MUST still be permitted by the receipt (keyed by the
	// stable v2 ref). The bug: a headKey-keyed receipt is orphaned here -> "SEAL REFUSED" -> head never advances.
	if err := os.WriteFile(filepath.Join(src, "f.bin"), append(body, 'y', 'z'), 0o600); err != nil {
		t.Fatal(err)
	}
	pushProfile(base, name, pass, src)
	if h2 := getRef(base, v2); h2 == h1 {
		t.Fatal("seal #2 after migration was REFUSED -- receipt orphaned by the head-key flip (#80/#72 regression)")
	}
}
