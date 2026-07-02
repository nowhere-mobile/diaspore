package main

import "testing"

// normRegion must default only a blank region; an explicit value (notably R2's "auto") passes through, or
// presigning/signing against Cloudflare R2 breaks.
func TestNormRegion(t *testing.T) {
	cases := map[string]string{
		"":             "us-east-1",    // blank -> default
		"auto":         "auto",         // Cloudflare R2 -- must NOT be remapped
		"us-west-004":  "us-west-004",  // Backblaze B2 zone
		"us-east-1":    "us-east-1",    // Filebase / AWS
		"eu-central-1": "eu-central-1", // arbitrary explicit
	}
	for in, want := range cases {
		if got := normRegion(in); got != want {
			t.Errorf("normRegion(%q) = %q, want %q", in, got, want)
		}
	}
}
