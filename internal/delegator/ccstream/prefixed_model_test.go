package ccstream

import "testing"

func TestPrefixedModel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"sonnet", "claude/sonnet"},
		{"claude-opus-4-8", "claude/claude-opus-4-8"},
		{"", ""},
		// The synthetic sentinel must pass through UNPREFIXED — a
		// "claude/<synthetic>" defeats every downstream exact-match guard and
		// bricks the session (regression guard for the 2026-07-13 incident that
		// the arnix model-prefixing commit re-armed).
		{syntheticModel, syntheticModel},
	}
	for _, c := range cases {
		if got := prefixedModel("claude", c.in); got != c.want {
			t.Errorf("prefixedModel(claude, %q) = %q, want %q", c.in, got, c.want)
		}
	}
}
