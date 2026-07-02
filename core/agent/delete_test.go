package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newHTTPStore stands up a tiny content-addressed store speaking the shape the non-s3 code path uses:
// POST /blob (-> sha256 key), GET /blob/<key>, GET|PUT /ref/<ref>. A tombstoned ref serves 200 + "" (so
// getRef reads back ""), mirroring delRef's overwrite-to-empty. Returns the base URL.
func newHTTPStore(t *testing.T) string {
	t.Helper()
	var mu sync.Mutex
	blobs := map[string][]byte{}
	refs := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/blob":
			body, _ := io.ReadAll(r.Body)
			h := sha256.Sum256(body)
			key := hex.EncodeToString(h[:])
			blobs[key] = body
			io.WriteString(w, key)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/blob/"):
			if b, ok := blobs[strings.TrimPrefix(r.URL.Path, "/blob/")]; ok {
				w.Write(b)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/ref/"):
			body, _ := io.ReadAll(r.Body)
			refs[strings.TrimPrefix(r.URL.Path, "/ref/")] = string(body)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/ref/"):
			delete(refs, strings.TrimPrefix(r.URL.Path, "/ref/")) // HARD-remove (not tombstone) -> simulates a LOST head
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/ref/"):
			if v, ok := refs[strings.TrimPrefix(r.URL.Path, "/ref/")]; ok {
				io.WriteString(w, v) // a tombstone is "" -> getRef reads ""
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// deleteProfile is the shared primitive behind the `delete-profile` CLI verb and the daemon DELETE verb.
// Security property: it removes a profile's head AND recovery refs ONLY when (name,pass) resolves, so a
// WRONG passphrase deletes nothing.
func TestDeleteProfileWrongPassIsNoop(t *testing.T) {
	base := newHTTPStore(t)
	name, pass := "alice", "correct horse battery"
	createVault(base, name, pass) // seeds the head ref + the recovery ref -> the vault blob

	head := getRef(base, headKey(base, name, pass))
	if head == "" {
		t.Fatal("setup: profile should resolve right after createVault")
	}
	v, isV := parseVault(getBlob(base, head))
	if !isV || v.RecoveryRef == "" {
		t.Fatal("setup: expected a vault head with a recovery ref")
	}

	// A wrong passphrase must NOT delete anything.
	if deleteProfile(base, name, "wrong-pass") {
		t.Fatal("deleteProfile returned true for a wrong passphrase")
	}
	if getRef(base, headKey(base, name, pass)) == "" {
		t.Fatal("a wrong passphrase wiped the head ref")
	}
	if getRef(base, v.RecoveryRef) == "" {
		t.Fatal("a wrong passphrase wiped the recovery ref")
	}

	// The right passphrase deletes both the head and the recovery refs.
	if !deleteProfile(base, name, pass) {
		t.Fatal("deleteProfile returned false for the correct passphrase")
	}
	if getRef(base, headKey(base, name, pass)) != "" {
		t.Fatal("head ref still resolves after delete")
	}
	if getRef(base, v.RecoveryRef) != "" {
		t.Fatal("recovery ref still resolves after delete (recovery path not killed)")
	}

	// Idempotent: a second delete is a no-op (already gone -> false).
	if deleteProfile(base, name, pass) {
		t.Fatal("second deleteProfile should be a no-op (false)")
	}
}

// The real-world bug: a deleted profile that is a LIVE roamed session gets sealed by the continuous syncLoop,
// and the seal (push) used to RE-CREATE the tombstoned head -> the profile could log in again. pushProfile
// must NOT resurrect a deleted bare (gate) profile, while still lazy-creating the "#de"/"#media" data refs.
func TestSealDoesNotResurrectDeletedProfile(t *testing.T) {
	base := newHTTPStore(t)
	name, pass := "bob", "hunter2 horse battery"
	createVault(base, name, pass)
	if getRef(base, headKey(base, name, pass)) == "" {
		t.Fatal("setup: profile should resolve")
	}

	// A normal seal of a LIVE profile updates it in place (head stays resolvable).
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	pushProfile(base, name, pass, src)
	if getRef(base, headKey(base, name, pass)) == "" {
		t.Fatal("a seal of a live profile must keep it resolvable")
	}

	// Delete it, then seal again (as the live syncLoop would): the seal must NOT bring it back.
	if !deleteProfile(base, name, pass) {
		t.Fatal("delete should succeed")
	}
	if getRef(base, headKey(base, name, pass)) != "" {
		t.Fatal("head should be tombstoned right after delete")
	}
	pushProfile(base, name, pass, src) // a post-delete seal (the bug: this used to resurrect the head)
	if got := getRef(base, headKey(base, name, pass)); got != "" {
		t.Fatalf("seal RESURRECTED a deleted profile: head=%q, want empty", got)
	}

	// The "#de"/"#media" data refs are exempt -- they legitimately lazy-create on their first seal.
	pushProfile(base, name+"#media", pass, src)
	if getRef(base, headKey(base, name+"#media", pass)) == "" {
		t.Fatal("a #media data ref should lazy-create on first seal")
	}
}

// The flip side of the no-resurrect guard (#19, DIA-20260624-05): a LOST head -- the ref OBJECT is gone
// (churn / corruption / partial state), NOT tombstoned -- on a LIVE session must RECOVER on the next seal,
// not be skipped forever. The distinction is tombstone (delRef leaves an empty object that EXISTS) vs an
// ABSENT ref (not-found). A deleted profile is always tombstoned AND reaped (no live session), so this can
// never resurrect a delete.
func TestSealRecoversLostHead(t *testing.T) {
	base := newHTTPStore(t)
	name, pass := "dave", "lost horse battery staple"
	createVault(base, name, pass)
	if getRef(base, headKey(base, name, pass)) == "" {
		t.Fatal("setup: profile should resolve right after createVault")
	}

	// Simulate a LOST head: HARD-remove the ref object (404/not-found), distinct from a delete tombstone
	// (which leaves an empty object that EXISTS). The "#de"/"#media" data refs survive (as for t/t1234).
	req, _ := http.NewRequest(http.MethodDelete, base+"/ref/"+headKey(base, name, pass), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if refExists(base, headKey(base, name, pass)) {
		t.Fatal("setup: a LOST head must read as ABSENT (not a tombstone)")
	}
	if getRef(base, headKey(base, name, pass)) != "" {
		t.Fatal("setup: a lost head should resolve empty")
	}

	// A live session seals -> the lost head RECOVERS (re-created + resolvable), unlike a tombstoned delete.
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("recovered"), 0o600); err != nil {
		t.Fatal(err)
	}
	pushProfile(base, name, pass, src)
	if getRef(base, headKey(base, name, pass)) == "" {
		t.Fatal("a LOST head was NOT recovered by the live-session seal (#19 regression)")
	}

	// And a DELETE tombstone still does NOT recover (the safety property is preserved). Re-create, delete,
	// seal -> stays gone.
	name2, pass2 := "erin", "deleted horse battery staple"
	createVault(base, name2, pass2)
	if !deleteProfile(base, name2, pass2) {
		t.Fatal("delete should succeed")
	}
	pushProfile(base, name2, pass2, src)
	if getRef(base, headKey(base, name2, pass2)) != "" {
		t.Fatal("a tombstoned (deleted) profile must STAY gone after a seal")
	}
}

// deleteForSession backs the daemon DELETE verb. A "Delete this profile" on a session NOT backed by a stored
// profile -- a throwaway blind login, or a profile already deleted (re-entered via a stale-read login) -- is a
// no-op that carries on to the reap, NOT a "wrong passphrase" error. But a session WITH a real profile and a
// WRONG confirm passphrase must still fail (never silently no-op, or a mistyped confirm would "delete" a
// profile that survives -- the DIA-50 guard). (DIA-20260617-04)
func TestDeleteForSession(t *testing.T) {
	base := newHTTPStore(t)
	name, pass := "carol", "correct horse battery"
	createVault(base, name, pass)

	// Real profile + WRONG confirm passphrase (the LOGIN creds are correct) -> "wrongpass", nothing deleted.
	if got := deleteForSession(base, name, "wrong-pass", name, pass); got != "wrongpass" {
		t.Fatalf("real profile + wrong confirm: got %q, want \"wrongpass\"", got)
	}
	if getRef(base, headKey(base, name, pass)) == "" {
		t.Fatal("a wrong confirm passphrase deleted the profile")
	}

	// Real profile + CORRECT confirm passphrase -> "deleted".
	if got := deleteForSession(base, name, pass, name, pass); got != "deleted" {
		t.Fatalf("real profile + correct confirm: got %q, want \"deleted\"", got)
	}
	if getRef(base, headKey(base, name, pass)) != "" {
		t.Fatal("head still resolves after a successful delete")
	}

	// The profile is now GONE: re-deleting the SAME still-logged-in session (login creds == typed, but the
	// head no longer resolves) -> "noop" (carry on), NOT "wrongpass". This is the stale-head re-login case
	// that used to surface a misleading "couldn't delete -- check your passphrase".
	if got := deleteForSession(base, name, pass, name, pass); got != "noop" {
		t.Fatalf("already-deleted session: got %q, want \"noop\"", got)
	}

	// A genuine throwaway session (no login creds recorded at all) -> "noop".
	if got := deleteForSession(base, "ghost", "whatever", "", ""); got != "noop" {
		t.Fatalf("throwaway session: got %q, want \"noop\"", got)
	}
}
