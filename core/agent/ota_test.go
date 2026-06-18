package main

import "testing"

// semverLess underpins ota-check: a newer published OS version (store ref) vs the running /system stamp.
func TestSemverLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.1.0", "0.2.0", true},
		{"0.1.0", "0.1.0", false}, // equal is not "less" -> uptodate
		{"0.2.0", "0.1.0", false},
		{"0.1.0", "0.1.1", true},
		{"0.9.0", "0.10.0", true}, // numeric, not lexical
		{"1.0.0", "0.9.9", false},
		{"0.1", "0.1.0", false}, // missing field zero-pads
		{"0.1", "0.1.1", true},
		{"", "0.0.1", true},  // empty running -> treated as 0.0.0
		{"0.0.0", "", false}, // empty latest -> 0.0.0, not newer
	}
	for _, c := range cases {
		if got := semverLess(c.a, c.b); got != c.want {
			t.Errorf("semverLess(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}
