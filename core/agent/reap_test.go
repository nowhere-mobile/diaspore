package main

import "testing"

// The no-reboot logoff hands the user-0 chooser its teardown in two phases: SWITCH (to the gate, fast, before
// the S3 seal) then REAP (remove the user, after the seal). Each is handed out exactly once; SWITCH wins if
// both are somehow pending. This is what shrinks the "Logging off…" escape window to ~one poll (~1.5s).
func TestNextReapActionTwoPhase(t *testing.T) {
	reset := func() { pendingReapMu.Lock(); pendingSwitchUID = ""; pendingReapUID = ""; pendingReapMu.Unlock() }
	reset()
	defer reset()

	if got := nextReapAction(); got != "NONE" {
		t.Fatalf("idle: got %q want NONE", got)
	}

	// logout queues the SWITCH first (gate reclaims the foreground before the seal)
	pendingReapMu.Lock(); pendingSwitchUID = "10"; pendingReapMu.Unlock()
	if got := nextReapAction(); got != "SWITCH 10" {
		t.Fatalf("got %q want SWITCH 10", got)
	}
	if got := nextReapAction(); got != "NONE" {
		t.Fatalf("SWITCH handed out once: got %q want NONE", got)
	}

	// after the seal completes, the REAP is queued
	pendingReapMu.Lock(); pendingReapUID = "10"; pendingReapMu.Unlock()
	if got := nextReapAction(); got != "REAP 10" {
		t.Fatalf("got %q want REAP 10", got)
	}
	if got := nextReapAction(); got != "NONE" {
		t.Fatalf("REAP handed out once: got %q want NONE", got)
	}

	// if both are pending, SWITCH takes priority (never remove before switching away + sealing)
	pendingReapMu.Lock(); pendingSwitchUID = "11"; pendingReapUID = "11"; pendingReapMu.Unlock()
	if got := nextReapAction(); got != "SWITCH 11" {
		t.Fatalf("priority: got %q want SWITCH 11", got)
	}
	if got := nextReapAction(); got != "REAP 11" {
		t.Fatalf("then: got %q want REAP 11", got)
	}
}
