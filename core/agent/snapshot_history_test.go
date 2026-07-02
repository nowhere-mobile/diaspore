package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// #58 P1: the vault History field must (a) stay invisible to pre-#58 heads so their signatures still verify
// (omitempty), (b) be covered by the signature when present, and (c) be tamper-evident.
func TestVaultHistorySignatureCompat(t *testing.T) {
	dk := bytes.Repeat([]byte{7}, 32)
	slots := []keyslot{{Kind: "pass", Wrapped: "x"}}

	// pre-#58 head: no History -> signs + verifies, and "history" must be OMITTED from the serialized bytes
	// (so a head signed before #58 is byte-identical and its signature still verifies).
	v := &vault{V: 1, Slots: slots, Manifest: "m0", Version: 1}
	signVault(v, dk)
	if !verifyVault(v, dk) {
		t.Fatal("a head with no History must verify")
	}
	if bytes.Contains(serializeVault(&vault{V: 1, Slots: slots, Manifest: "m0", Version: 1}), []byte("history")) {
		t.Fatal("empty History must be omitted from the serialized header")
	}

	// history-bearing head: signs + verifies, and History IS part of the signed bytes
	v.History = []headSnap{{Version: 1, Manifest: "m0", Time: 100}}
	signVault(v, dk)
	if !verifyVault(v, dk) {
		t.Fatal("a History-bearing head must verify")
	}
	if !bytes.Contains(serializeVault(v), []byte("history")) {
		t.Fatal("non-empty History must be present in the signed header")
	}

	// tamper: altering a retained snapshot must break the signature (rollback targets are protected)
	v.History[0].Manifest = "evil"
	if verifyVault(v, dk) {
		t.Fatal("altering a History entry must invalidate the signature")
	}
}

// pushSnapshot records the OUTGOING head only when the data actually changed, tagged with its kind.
func TestPushSnapshotRecordsOnChange(t *testing.T) {
	v := &vault{Manifest: ""}
	pushSnapshot(v, "m1", "") // empty prior head -> nothing to snapshot
	if len(v.History) != 0 {
		t.Fatalf("an empty prior head must not snapshot: got %d", len(v.History))
	}
	v.Version, v.Manifest = 1, "m1"
	pushSnapshot(v, "m1", "") // no-change re-seal -> nothing
	if len(v.History) != 0 {
		t.Fatalf("a no-change re-seal must not snapshot: got %d", len(v.History))
	}
	pushSnapshot(v, "m2", "manual") // real change -> records the outgoing head m1 with its kind
	if len(v.History) != 1 || v.History[0].Manifest != "m1" || v.History[0].Kind != "manual" {
		t.Fatalf("a real change must snapshot the outgoing head m1/manual: got %+v", v.History)
	}
}

// #58 two-class retention: MANUAL saves are pinned (newest snapManualMax), AUTOMATIC saves are time-spaced
// (recent + hourly + daily, capped), merged newest-first by version.
func TestPruneSnapshotsTwoClass(t *testing.T) {
	t.Setenv("NOWHERE_SNAP_MANUAL", "2")      // keep the newest 2 manual (pinned)
	t.Setenv("NOWHERE_SNAP_AUTO", "3")        // up to 3 auto
	t.Setenv("NOWHERE_SNAP_AUTO_RECENT", "1") // 1 recent auto, then hourly/daily
	t.Setenv("NOWHERE_SNAP_AUTO_HOURS", "6")
	t.Setenv("NOWHERE_SNAP_AUTO_DAYS", "10")
	const hour, day = int64(3600), int64(86400)
	now := int64(1000) * day

	hist := []headSnap{ // newest-first by Version
		{Version: 30, Time: now - 60, Kind: ""},          // auto, ~now      -> recent auto (kept)
		{Version: 29, Time: now - 2*hour, Kind: ""},      // auto, 2h ago    -> hourly (kept)
		{Version: 28, Time: now - 3*hour, Kind: ""},      // auto, 3h ago    -> hourly (kept -> auto cap 3 hit)
		{Version: 27, Time: now - 4*hour, Kind: ""},      // auto, 4h ago    -> DROPPED (auto cap)
		{Version: 26, Time: now - 1*day, Kind: "manual"}, // manual          -> pinned (kept)
		{Version: 25, Time: now - 2*day, Kind: "manual"}, // manual          -> pinned (kept -> manual cap 2 hit)
		{Version: 24, Time: now - 3*day, Kind: "manual"}, // manual          -> DROPPED (manual cap)
	}
	got := pruneSnapshots(hist, now)

	var keep []uint64
	for _, s := range got {
		keep = append(keep, s.Version)
	}
	want := []uint64{30, 29, 28, 26, 25} // 3 auto + 2 manual, merged newest-first
	if fmt.Sprint(keep) != fmt.Sprint(want) {
		t.Fatalf("two-class retention wrong:\n got  %v\n want %v", keep, want)
	}
	// a burst of AUTO churn must never evict a pinned MANUAL save
	churn := []headSnap{}
	for i := 0; i < 50; i++ {
		churn = append([]headSnap{{Version: uint64(100 + i), Time: now + int64(i), Kind: ""}}, churn...)
	}
	churn = append(churn, headSnap{Version: 5, Time: now - 4*day, Kind: "manual"})
	out := pruneSnapshots(churn, now)
	found := false
	for _, s := range out {
		if s.Version == 5 && s.Kind == "manual" {
			found = true
		}
	}
	if !found {
		t.Fatal("a pinned manual save must survive a burst of automatic churn")
	}
}

// rollbackHead repoints the head to a retained snapshot as a FORWARD write, and restoring it yields the
// earlier data -- the end-to-end #58 recovery path.
func TestRollbackHeadRestoresEarlierData(t *testing.T) {
	t.Setenv("NOWHERE_SNAP_AUTO_RECENT", "5") // keep all the (auto) test seals so v1 is retained
	base := newHTTPStore(t)
	name, pass := "snap", "correct horse battery staple"
	createVault(base, name, pass)

	src := t.TempDir()
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(src, "note.txt"), []byte(s), 0644); err != nil {
			t.Fatal(err)
		}
	}

	write("VERSION ONE")
	pushProfile(base, name, pass, src) // -> head carrying manifest M1
	v1, _ := parseVault(getBlob(base, getRef(base, headKey(base, name, pass))))
	m1, ver1 := v1.Manifest, v1.Version

	write("VERSION TWO IS DIFFERENT")
	pushProfile(base, name, pass, src) // -> head carrying manifest M2; M1 retained in History
	v2, _ := parseVault(getBlob(base, getRef(base, headKey(base, name, pass))))
	if v2.Manifest == m1 {
		t.Fatal("v2 manifest must differ from v1")
	}
	retained := false
	for _, s := range v2.History {
		if s.Manifest == m1 && s.Version == ver1 {
			retained = true
		}
	}
	if !retained {
		t.Fatalf("v1 (v%d) must be retained in History, got %+v", ver1, v2.History)
	}

	// roll back to v1's snapshot -> a FORWARD write (version bumps, not regresses)
	nv, err := rollbackHead(base, name, pass, ver1)
	if err != nil {
		t.Fatal(err)
	}
	if nv != v2.Version+1 {
		t.Fatalf("rollback must be a forward write: got v%d, want v%d", nv, v2.Version+1)
	}
	v3, _ := parseVault(getBlob(base, getRef(base, headKey(base, name, pass))))
	if v3.Manifest != m1 {
		t.Fatalf("rolled-back head must carry v1's manifest %s, got %s", short(m1), short(v3.Manifest))
	}

	// and restoring the rolled-back head yields v1's data
	_, dst := restoreManifest(t, base, name, pass)
	got, err := os.ReadFile(filepath.Join(dst, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "VERSION ONE" {
		t.Fatalf("restore after rollback must yield v1 data, got %q", got)
	}
}
