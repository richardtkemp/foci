package agent

import (
	"strings"
	"testing"

	"foci/internal/usagehold"
)

// TestUnavailable names each wait a sender is told about (#2892), and reads an
// agent with none, an API agent during a login included, as available.
func TestUnavailable(t *testing.T) {
	reg := usagehold.New()
	a := holdAgent(reg, "claude-code")
	if why, un := a.Unavailable(); un {
		t.Fatalf("open account: unavailable %q", why)
	}
	a.ObserveUsage(weekly(0.99))
	if why, un := a.Unavailable(); !un || !strings.Contains(why, "usage hold") {
		t.Errorf("in a hold: %q %v, want the hold named", why, un)
	}
	a.ObserveUsage(weekly(0.10))

	claimRelogin(t)
	if why, un := a.Unavailable(); !un || !strings.Contains(why, "login") {
		t.Errorf("in a login: %q %v, want the login named", why, un)
	}
	if why, un := (&Agent{}).Unavailable(); un {
		t.Errorf("an API agent in a login: unavailable %q, want available", why)
	}
}

// TestSessionLastActivity_NoBackend: no live backend reads as zero and creates
// nothing.
func TestSessionLastActivity_NoBackend(t *testing.T) {
	if got := (&Agent{}).SessionLastActivity("a/c1"); !got.IsZero() {
		t.Errorf("API agent: %v, want zero", got)
	}
	a := holdAgent(usagehold.New(), "claude-code")
	if got := a.SessionLastActivity("a/c1"); !got.IsZero() {
		t.Errorf("no backend: %v, want zero", got)
	}
}
