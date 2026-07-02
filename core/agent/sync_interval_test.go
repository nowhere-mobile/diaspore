package main

import "testing"

// clampSyncInterval keeps the user-settable seal cadence in [15s, 1 day].
func TestClampSyncInterval(t *testing.T) {
	cases := map[int]int{
		1:       15,    // below floor -> floor
		14:      15,    // just below floor
		15:      15,    // floor
		30:      30,    // a preset
		120:     120,   // the default
		300:     300,   // 5 min preset
		86400:   86400, // ceiling (1 day)
		1000000: 86400, // above ceiling -> ceiling
	}
	for in, want := range cases {
		if got := clampSyncInterval(in); got != want {
			t.Errorf("clampSyncInterval(%d) = %d, want %d", in, got, want)
		}
	}
}
