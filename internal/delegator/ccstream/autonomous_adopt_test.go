package ccstream

import (
	"bytes"
	"sync"
	"testing"

	"foci/internal/delegator"
)

// TestEdgeCallbacks_FIFOOrderUnderConcurrentDrain pins the drain ordering: edge
// callbacks (the autonomous-open enqueued at the running edge, #1261) are
// enqueued under turnMu and drained under fireMu in FIFO order, exactly once
// each, even with many goroutines racing to drain.
func TestEdgeCallbacks_FIFOOrderUnderConcurrentDrain(t *testing.T) {
	t.Parallel()

	b := &Backend{}
	var mu sync.Mutex
	var order []string
	fired := 0

	b.turnMu.Lock()
	b.edgeCallbacks = append(b.edgeCallbacks, func() { mu.Lock(); order = append(order, "a"); fired++; mu.Unlock() })
	b.edgeCallbacks = append(b.edgeCallbacks, func() { mu.Lock(); order = append(order, "b"); fired++; mu.Unlock() })
	b.turnMu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); b.drainEdgeCallbacks() }()
	}
	wg.Wait()

	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("edge fire order = %v, want [a b] exactly once each", order)
	}
	if fired != 2 {
		t.Fatalf("fired = %d, want 2", fired)
	}
}

// TestAutonomousOpenCallback pins #1261: the backend fires onAutonomousOpen
// exactly when it detects CC has begun a run foci did not open
// (session_state:running with no foci turn), so the agent can adopt it as a
// first-class turn.
func TestAutonomousOpenCallback(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}

	var opens int
	b.SetOnAutonomousOpen(func() { opens++ })

	// No foci turn open → running is a CC-initiated run → onAutonomousOpen fires.
	stateEvent(b, "running")
	if opens != 1 {
		t.Fatalf("after autonomous running: opens=%d, want 1", opens)
	}

	// Idle with no adopted turn (the stub opened none) → nothing further fires.
	stateEvent(b, "idle")
	if opens != 1 {
		t.Fatalf("after idle: opens=%d, want 1", opens)
	}
}

// TestAutonomousOpen_NotFiredForFociTurn confirms onAutonomousOpen is scoped to
// CC-initiated runs: a normal foci turn (turnActive=true) must NOT fire it — its
// lifecycle is owned by OrchestrateFullTurn.
func TestAutonomousOpen_NotFiredForFociTurn(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}

	var opens int
	b.SetOnAutonomousOpen(func() { opens++ })

	handler := &testHandler{OnTurnComplete: func(*delegator.TurnResult) {}}
	applyHandler(b, handler) // opens a real foci turn (turnActive=true)
	stateEvent(b, "running")
	stateEvent(b, "idle")

	if opens != 0 {
		t.Fatalf("foci turn must not fire onAutonomousOpen; opens=%d", opens)
	}
}
