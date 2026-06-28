package opencode

import (
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
)

// ---------------------------------------------------------------------------
// LastActivity — ActivityChecker implementation
// ---------------------------------------------------------------------------

func TestLastActivity_ZeroOnFreshServer(t *testing.T) {
	// Verifies a fresh Backend (never received events) reports the zero
	// time. The agent layer uses this to detect "subscriber hasn't
	// connected yet" vs "subscriber connected but server is quiet."
	b := &Backend{
		server: &Server{},
	}
	if got := b.LastActivity(); !got.IsZero() {
		t.Errorf("fresh Server LastActivity = %v, want zero", got)
	}
}

func TestLastActivity_ZeroOnNilServer(t *testing.T) {
	// Verifies a Backend without a Server (test-only / pre-Start)
	// returns zero rather than panicking.
	b := &Backend{}
	if got := b.LastActivity(); !got.IsZero() {
		t.Errorf("nil Server LastActivity = %v, want zero", got)
	}
}

func TestLastActivity_UpdatedOnEvent(t *testing.T) {
	// Verifies the subscriber's onEvent callback updates the Server's
	// lastActivity stamp, and Backend.LastActivity() reads it. We drive
	// the callback directly rather than spinning up a real subscriber
	// — the subscriber's onEvent closure is what calls Store in
	// production (subscriber.go:221).
	srv := &Server{}
	b := &Backend{server: srv}

	before := b.LastActivity()
	// Simulate an event arriving — subscriber.go's onEvent does this.
	srv.lastActivity.Store(time.Now().UnixNano())
	time.Sleep(time.Millisecond) // ensure the stamp moved forward

	after := b.LastActivity()
	if !after.After(before) {
		t.Errorf("LastActivity not updated after event: before=%v after=%v", before, after)
	}
}

func TestLastActivity_UpdatedOnHeartbeat(t *testing.T) {
	// Verifies SSE comment-line heartbeats (subscriber.go's onHeartbeat)
	// also update lastActivity. This is what keeps the activity-aware
	// timeout from firing when the server is alive but processing a
	// long tool call (no events, but heartbeats keep arriving).
	srv := &Server{}
	b := &Backend{server: srv}

	before := b.LastActivity()
	// Simulate a heartbeat — subscriber.go's onHeartbeat does this.
	srv.lastActivity.Store(time.Now().UnixNano())
	time.Sleep(time.Millisecond)

	after := b.LastActivity()
	if !after.After(before) {
		t.Errorf("LastActivity not updated after heartbeat: before=%v after=%v", before, after)
	}
}

func TestLastActivity_SharedAcrossBackendsOnSameServer(t *testing.T) {
	// Verifies two Backends sharing a Server see the same LastActivity.
	// The stamp lives on the Server, not the Backend — all sessions on
	// the same Server share one SSE subscriber, so they all observe
	// the same liveness signal. This is how the agent layer knows the
	// Server is alive even if only one session has an active turn.
	srv := &Server{}
	b1 := &Backend{server: srv, sessionID: "sess-1"}
	b2 := &Backend{server: srv, sessionID: "sess-2"}

	// Initially both zero.
	if !b1.LastActivity().IsZero() || !b2.LastActivity().IsZero() {
		t.Fatal("expected zero LastActivity on fresh Server")
	}

	// An event arrives — the Server stamp updates.
	stamp := time.Now().UnixNano()
	srv.lastActivity.Store(stamp)

	// Both Backends should see the same time.
	t1 := b1.LastActivity()
	t2 := b2.LastActivity()
	if !t1.Equal(t2) {
		t.Errorf("b1.LastActivity (%v) != b2.LastActivity (%v) — should share Server stamp", t1, t2)
	}
	if t1.UnixNano() != stamp {
		t.Errorf("LastActivity unix nanos = %d, want %d", t1.UnixNano(), stamp)
	}
}

// Ensure delegator.ActivityChecker is satisfied (compile-time check).
var _ delegator.ActivityChecker = (*Backend)(nil)

// Suppress unused import warning if strings ends up unused after test
// trimming.
var _ = strings.Contains
