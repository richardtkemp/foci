package main

import (
	"strings"
	"testing"
)

// TestSessionFlagStripArgs unit-pins the shared parser's extract-style entry
// point: every -s/--session shape is removed wherever it sits, a valueless
// trailing flag is left for the positionals, and the last value wins.
func TestSessionFlagStripArgs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantSel  string
		wantRest []string
	}{
		{"no flag", []string{"a", "b"}, "", []string{"a", "b"}},
		{"spaced", []string{"-s", "x", "a"}, "x", []string{"a"}},
		{"long spaced", []string{"--session", "x", "a"}, "x", []string{"a"}},
		{"short equals", []string{"-s=x", "a"}, "x", []string{"a"}},
		{"long equals", []string{"--session=x", "a"}, "x", []string{"a"}},
		{"mid-args", []string{"a", "-s", "x", "b"}, "x", []string{"a", "b"}},
		{"last wins", []string{"-s", "one", "a", "--session=two"}, "two", []string{"a"}},
		{"valueless trailing flag stays", []string{"a", "-s"}, "", []string{"a", "-s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sf sessionFlag
			rest := sf.stripArgs(tc.args)
			if sf.session != tc.wantSel {
				t.Errorf("selector = %q, want %q", sf.session, tc.wantSel)
			}
			if strings.Join(rest, " ") != strings.Join(tc.wantRest, " ") {
				t.Errorf("rest = %v, want %v", rest, tc.wantRest)
			}
		})
	}
}
