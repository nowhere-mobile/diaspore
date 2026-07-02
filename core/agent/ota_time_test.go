package main

import "testing"

func TestValidOtaTime(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"03:00", "03:00"},
		{"3:5", "03:05"},   // normalised
		{"23:59", "23:59"},
		{"00:00", "00:00"},
		{"", ""},           // empty = any-time
		{"24:00", ""},      // hour out of range
		{"12:60", ""},      // minute out of range
		{"-1:00", ""},      // negative
		{"noon", ""},       // garbage
		{"1200", ""},       // missing colon
	}
	for _, c := range cases {
		if got := validOtaTime(c.in); got != c.want {
			t.Errorf("validOtaTime(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
