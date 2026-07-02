package main

import (
	"errors"
	"testing"
)

// TestRetryStore: a transient store error (a Wi-Fi blip / Filebase 5xx mid-seal) must be RETRIED, not allowed
// to abort the operation -- this is the silent large-seal-loss fix (DIA-20260625-01: pushProfile uploads
// every chunk before writing the ref, so one un-retried PUT error discarded the whole seal). A
// persistently-down store must still surface an error, so a real outage fails LOUDLY instead of silently
// dropping the seal.
func TestRetryStore(t *testing.T) {
	old := s3RetryBase
	s3RetryBase = 0 // no backoff sleeps in the test
	defer func() { s3RetryBase = old }()

	// Recovers: fails twice (transient), succeeds on the 3rd attempt.
	n := 0
	if err := retryStore("flaky", func() error {
		n++
		if n < 3 {
			return errors.New("connection reset by peer")
		}
		return nil
	}); err != nil {
		t.Fatalf("retryStore should recover from transient failures, got %v (after %d tries)", err, n)
	}
	if n != 3 {
		t.Fatalf("want 3 attempts (2 transient + 1 success), got %d", n)
	}

	// Gives up: a store that never recovers must return the LAST error (loud failure) after the cap, so a
	// genuinely-down store can't masquerade as a successful seal.
	m := 0
	if err := retryStore("down", func() error {
		m++
		return errors.New("store unreachable")
	}); err == nil {
		t.Fatal("a persistently-failing store must surface an error, not silently succeed (that would drop a seal)")
	}
	if m != 5 {
		t.Fatalf("want 5 attempts before giving up, got %d", m)
	}
}
