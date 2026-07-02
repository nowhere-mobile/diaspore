package main

import (
	"net/http"
	"testing"
)

// #71 step 1: the restore is latency-bound (an EU presign round-trip per chunk), so the fetch concurrency is
// bumped well above the old 8, and the keep-alive pool is raised so those workers reuse connections instead of
// handshaking per chunk. Env still overrides the default.
func TestDownloadWorkersTuned(t *testing.T) {
	t.Setenv("NOWHERE_DOWNLOAD_WORKERS", "") // empty -> the default path
	if got := downloadWorkers(); got < 16 {
		t.Fatalf("download concurrency should be bumped for the latency-bound restore, got %d", got)
	}
	t.Setenv("NOWHERE_DOWNLOAD_WORKERS", "40")
	if got := downloadWorkers(); got != 40 {
		t.Fatalf("env override should win, got %d", got)
	}
}

func TestKeepAlivePoolRaised(t *testing.T) {
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Skip("DefaultTransport is not *http.Transport on this build")
	}
	if tr.MaxIdleConnsPerHost < 24 {
		t.Fatalf("keep-alive pool (%d) is smaller than the restore concurrency -> workers churn TLS handshakes", tr.MaxIdleConnsPerHost)
	}
}
