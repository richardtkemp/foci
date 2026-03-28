package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/config"
)

// TestAPITransport_IncrementProcessing verifies the atomic counter goes up and back down.
func TestAPITransport_IncrementProcessing(t *testing.T) {
	a := &Agent{}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)

	if a.IsProcessing() {
		t.Fatal("should not be processing before IncrementProcessing")
	}

	dec := tr.IncrementProcessing(ts)
	if !a.IsProcessing() {
		t.Fatal("should be processing after IncrementProcessing")
	}
	if got := atomic.LoadInt32(&a.processing); got != 1 {
		t.Fatalf("processing = %d, want 1", got)
	}

	dec()
	if a.IsProcessing() {
		t.Fatal("should not be processing after decrement")
	}
}

// TestAPITransport_RegisterTurn verifies turn detail registration and cleanup.
func TestAPITransport_RegisterTurn(t *testing.T) {
	a := &Agent{}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.Trigger = "telegram"

	unreg := tr.RegisterTurn(ts)

	if ts.TurnDetail == nil {
		t.Fatal("TurnDetail should be set")
	}
	if ts.TurnDetail.SessionKey != "test/s" {
		t.Errorf("TurnDetail.SessionKey = %q, want %q", ts.TurnDetail.SessionKey, "test/s")
	}
	if ts.TurnDetail.Trigger != "telegram" {
		t.Errorf("TurnDetail.Trigger = %q, want %q", ts.TurnDetail.Trigger, "telegram")
	}

	details := a.ProcessingDetails()
	if len(details) != 1 {
		t.Fatalf("ProcessingDetails len = %d, want 1", len(details))
	}

	unreg()
	details = a.ProcessingDetails()
	if len(details) != 0 {
		t.Fatalf("ProcessingDetails len = %d after unreg, want 0", len(details))
	}
}

// TestAPITransport_AcquireTurnLock verifies serialization and unlock.
func TestAPITransport_AcquireTurnLock(t *testing.T) {
	a := &Agent{}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.Trigger = "telegram"

	unlock := tr.AcquireTurnLock(ts)

	// Lock is held — a second goroutine should block.
	blocked := make(chan struct{})
	go func() {
		ts2 := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
		ts2.Trigger = "keepalive"
		unlock2 := tr.AcquireTurnLock(ts2)
		close(blocked)
		unlock2()
	}()

	select {
	case <-blocked:
		t.Fatal("second lock should block while first is held")
	case <-time.After(50 * time.Millisecond):
		// expected
	}

	unlock()

	select {
	case <-blocked:
		// expected — second lock acquired after unlock
	case <-time.After(5 * time.Second):
		t.Fatal("second lock should acquire after first unlocks")
	}
}

// TestAPITransport_ResolveModelEffort verifies model/effort/thinking/speed resolution.
func TestAPITransport_ResolveModelEffort(t *testing.T) {
	a := &Agent{
		Model: "anthropic/claude-sonnet-4-20250514",
	}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)

	tr.ResolveModelEffort(ts)

	if ts.TurnModel != "anthropic/claude-sonnet-4-20250514" {
		t.Errorf("TurnModel = %q, want agent default", ts.TurnModel)
	}
	// No per-model defaults configured, so effort/thinking/speed stay empty.
	if ts.TurnEffort != "" {
		t.Errorf("TurnEffort = %q, want empty", ts.TurnEffort)
	}
}

// TestAPITransport_ResolveModelEffort_WithDefaults verifies model defaults apply.
func TestAPITransport_ResolveModelEffort_WithDefaults(t *testing.T) {
	a := &Agent{
		Model: "anthropic/claude-sonnet-4-20250514",
		ModelDefaultsFn: func(model string) config.ModelDefaults {
			return config.ModelDefaults{Effort: "high", Thinking: "adaptive", Speed: ""}
		},
	}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)

	tr.ResolveModelEffort(ts)

	if ts.TurnEffort != "high" {
		t.Errorf("TurnEffort = %q, want %q", ts.TurnEffort, "high")
	}
	if ts.TurnThinking != "adaptive" {
		t.Errorf("TurnThinking = %q, want %q", ts.TurnThinking, "adaptive")
	}
}
