package main

import (
	"testing"
)

// TestScoreSessionFlagSharedParser pins that `foci score` parses -s/--session
// through the one shared sessionFlag parser (#2284): all four flag forms and
// the FOCI_SESSION fallback work, and a repeated flag keeps the LAST value
// with no flag word leaking into the positional name/value — the same rule
// send and branch follow. On the old private parser a second -s stayed in
// the positional args and became the score's name.
func TestScoreSessionFlagSharedParser(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"-s value", []string{"-s", "clutch/c123", "quality", "4"}, "clutch/c123"},
		{"-s=value", []string{"-s=clutch/c123", "quality", "4"}, "clutch/c123"},
		{"--session value", []string{"--session", "clutch/c123", "quality", "4"}, "clutch/c123"},
		{"--session=value", []string{"--session=clutch/c123", "quality", "4"}, "clutch/c123"},
		{"flag after positionals", []string{"quality", "4", "-s", "clutch/c123"}, "clutch/c123"},
		{"repeated flag keeps the last value", []string{"-s", "first", "-s", "clutch/c123", "quality", "4"}, "clutch/c123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := captureBody(t, func(base string) error {
				return cmdScore(base, tc.args)
			})
			if body["session"] != tc.want {
				t.Errorf("session = %v, want %v", body["session"], tc.want)
			}
			if body["name"] != "quality" || body["value"] != "4" {
				t.Errorf("name/value = %v/%v, want quality/4 — a session flag word leaked into the positionals", body["name"], body["value"])
			}
		})
	}

	t.Run("FOCI_SESSION alone sets it", func(t *testing.T) {
		t.Setenv("FOCI_SESSION", "clutch/c9")
		body := captureBody(t, func(base string) error {
			return cmdScore(base, []string{"quality", "4"})
		})
		if body["session"] != "clutch/c9" {
			t.Errorf("session = %v, want clutch/c9 from FOCI_SESSION", body["session"])
		}
	})

	t.Run("flag beats the env", func(t *testing.T) {
		t.Setenv("FOCI_SESSION", "clutch/c9")
		body := captureBody(t, func(base string) error {
			return cmdScore(base, []string{"-s", "clutch/c123", "quality", "4"})
		})
		if body["session"] != "clutch/c123" {
			t.Errorf("session = %v, want clutch/c123 — the flag must win over FOCI_SESSION", body["session"])
		}
	})

	t.Run("no flag and no env leave it empty", func(t *testing.T) {
		t.Setenv("FOCI_SESSION", "")
		body := captureBody(t, func(base string) error {
			return cmdScore(base, []string{"quality", "4"})
		})
		if body["session"] != "" {
			t.Errorf("session = %v, want empty", body["session"])
		}
	})
}
