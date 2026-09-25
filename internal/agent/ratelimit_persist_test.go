package agent

import (
	"path/filepath"
	"testing"
	"time"

	"foci/internal/ratelimit"
	"foci/internal/session"
)

// #2026: an endpoint's rate-limit deadline survives a restart. Before, a fresh
// process booted every gate open, so all the periodic schedulers fired into a
// live cap until one of them tripped it again. Each "restart" is a new Agent
// over the same state.db.
func TestRateLimitGate_DeadlineSurvivesRestart(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	boot := func() *Agent { return &Agent{AgentID: "ag", Endpoint: "anthropic", SessionIndex: idx} }

	resetAt := time.Now().Add(time.Hour)
	boot().engageRateLimit("anthropic", ratelimit.Signal{Kind: ratelimit.KindUsage, ResetAt: resetAt}, false)

	limited, until := boot().getOrCreateRateLimitGate("anthropic").IsLimited()
	if !limited || !until.Equal(resetAt) {
		t.Fatalf("after restart: limited=%v until=%v, want closed until %v", limited, until, resetAt)
	}
	// The default endpoint ("" = Agent.Endpoint) resolves to the same row.
	if limited, _ := boot().getOrCreateRateLimitGate("").IsLimited(); !limited {
		t.Error("default-endpoint gate not restored")
	}
	// Another endpoint is unaffected.
	if limited, _ := boot().getOrCreateRateLimitGate("gemini").IsLimited(); limited {
		t.Error("an unrelated endpoint's gate came back closed")
	}

	// A successful probe releases the gate; the release must survive too, or
	// the next process would re-close a gate the API already reopened.
	boot().releaseRateLimit("anthropic")
	if limited, _ := boot().getOrCreateRateLimitGate("anthropic").IsLimited(); limited {
		t.Error("released gate came back closed after restart")
	}
}
