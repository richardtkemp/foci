package codex

import "testing"

// TestFireWarning mirrors ccstream's TestFireAuthFailure: the fire helper
// must be nil-safe (no panic without a hook) and must deliver its detail
// string to a registered hook. There is no auth-failure string matcher in
// the codex backend (that is CC-specific retry logic), so only the generic
// hook-dispatch contract is ported here.
func TestFireWarning(t *testing.T) {
	b := &Backend{}
	// No hook set → no panic.
	b.fireWarning("x")

	var got string
	b.onWarning = func(detail string) { got = detail }
	b.fireWarning("config warning")
	if got != "config warning" {
		t.Errorf("hook got %q", got)
	}
}
