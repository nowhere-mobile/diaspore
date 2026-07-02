package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// setEnrollEnv points the limiter at a throwaway state file and fixes its capacity/window.
func setEnrollEnv(t *testing.T, max, window string) string {
	t.Helper()
	state := filepath.Join(t.TempDir(), "enroll")
	t.Setenv("NOWHERE_ENROLL_STATE", state)
	t.Setenv("NOWHERE_ENROLL_MAX", max)
	t.Setenv("NOWHERE_ENROLL_WINDOW", window)
	return state
}

// A fresh device starts with a full bucket (MAX creates), then denies the next one with a sane retry.
func TestEnrollBurstThenThrottle(t *testing.T) {
	setEnrollEnv(t, "3", "3600")
	for i := 0; i < 3; i++ {
		if ok, _ := enrollAllow(); !ok {
			t.Fatalf("create %d should be allowed from a full bucket", i+1)
		}
	}
	ok, retry := enrollAllow()
	if ok {
		t.Fatal("the 4th create should be throttled (bucket empty)")
	}
	// refill rate = 3/3600 = 1 token / 1200s, so the wait is ~1200s.
	if retry < 1100 || retry > 1300 {
		t.Fatalf("retry-after %d out of the expected ~1200s range", retry)
	}
}

// MAX<=0 disables the limiter entirely (turnkey / trusted deployments).
func TestEnrollDisabled(t *testing.T) {
	setEnrollEnv(t, "0", "3600")
	for i := 0; i < 50; i++ {
		if ok, _ := enrollAllow(); !ok {
			t.Fatalf("limiter should be disabled at MAX=0 (denied at %d)", i)
		}
	}
}

// Tokens accrue over elapsed time: a drained bucket whose timestamp is one window old refills to full.
func TestEnrollRefillOverTime(t *testing.T) {
	state := setEnrollEnv(t, "5", "1000")
	// Hand-write a drained bucket stamped one full window in the past.
	old := enrollBucket{Tokens: 0, TS: time.Now().Unix() - 1000}
	raw, _ := json.Marshal(old)
	if err := os.WriteFile(state, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// One window of elapsed time = +5 tokens (capped at MAX); so 5 creates should now pass, the 6th not.
	for i := 0; i < 5; i++ {
		if ok, _ := enrollAllow(); !ok {
			t.Fatalf("create %d should pass after a full-window refill", i+1)
		}
	}
	if ok, _ := enrollAllow(); ok {
		t.Fatal("the 6th create should be throttled again (refill caps at MAX)")
	}
}

// The limit survives a "power-off": state on disk persists, so a flood can't be reset by restarting.
func TestEnrollPersists(t *testing.T) {
	setEnrollEnv(t, "2", "3600")
	enrollAllow()
	enrollAllow() // bucket now empty
	// Simulate a daemon restart: the next call reads the same on-disk state (no in-memory reset).
	if ok, _ := enrollAllow(); ok {
		t.Fatal("a restarted daemon must still see the drained bucket from disk")
	}
}
