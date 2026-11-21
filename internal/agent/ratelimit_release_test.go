package agent

import (
	"path/filepath"
	"testing"
	"time"

	"foci/internal/ratelimit"
	"foci/internal/session"
	"foci/internal/usagehold"
)

// TestReleaseRateLimit_OpensGateAndSurvivesRestart (#3239): a delegated
// backend reporting that its limit cleared (codex: credits added) opens the
// gate its limit closed, before the deadline, and clears the stored deadline
// so a restart comes up open. Releasing an open gate changes nothing.
func TestReleaseRateLimit_OpensGateAndSurvivesRestart(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	reg := usagehold.New()
	boot := func() *Agent {
		a := holdAgent(reg, "codex")
		a.Endpoint = ""
		a.SessionIndex = idx
		return a
	}
	const sk = "codex-agent/c1"

	a := boot()
	a.EngageRateLimit(testAcct(a), ratelimit.Signal{Kind: ratelimit.KindUsage, ResetAt: time.Now().Add(time.Hour)})
	if lim, _ := a.SessionRateLimited(sk); !lim {
		t.Fatal("gate open after EngageRateLimit, want closed")
	}

	a.ReleaseRateLimit(testAcct(a))
	if lim, reason := a.SessionRateLimited(sk); lim {
		t.Fatalf("after ReleaseRateLimit: still blocked (%s)", reason)
	}

	a = boot()
	if lim, reason := a.SessionRateLimited(sk); lim {
		t.Fatalf("after release + restart: still blocked (%s)", reason)
	}

	a.ReleaseRateLimit(testAcct(a)) // already open: a no-op
	if lim, reason := a.SessionRateLimited(sk); lim {
		t.Fatalf("release of an open gate closed it (%s)", reason)
	}
}
