package ccstream

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

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

// TestAutonomousOpen_NotFiredForCompactInject pins #1266: the fire-and-forget
// /compact slash command (SourceCompact) must be recognised as foci-initiated,
// exactly like a real foci turn — its "running" transition must NOT fire
// onAutonomousOpen. Before the fix, ImmediateInject(SourceCompact) wrote
// straight to CC via sendUserMessage without ever setting turnActive, so the
// running edge looked identical to a spontaneous CC-initiated run and got
// adopted — which clobbers the session router mid-compaction and causes the
// already-delivered pre-compaction reply to be re-sent as a duplicate message
// once the (much later) deferred TurnComplete falls through to the
// late-delivery fallback. See internal/agent/agent.go HandleMessage's
// deferred TurnComplete + internal/agent/in_flight.go OpenAutonomousTurn.
func TestAutonomousOpen_NotFiredForCompactInject(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}

	var opens int
	b.SetOnAutonomousOpen(func() { opens++ })

	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceCompact,
		Text:   "/compact summarise",
	}); err != nil {
		t.Fatalf("ImmediateInject(SourceCompact): %v", err)
	}

	// CC opens a run to process the /compact — foci sent it, so this must NOT
	// be treated as an autonomous (CC-initiated) run.
	stateEvent(b, "running")
	if opens != 0 {
		t.Fatalf("compact inject must not fire onAutonomousOpen; opens=%d", opens)
	}

	// Compaction completes — must not panic or wedge with a nil turnEvents.
	stateEvent(b, "idle")
	if opens != 0 {
		t.Fatalf("after idle: opens=%d, want 0", opens)
	}
	if b.IsTurnInFlight() {
		t.Fatalf("turn must be cleared after compact's idle, still active")
	}
}

// TestCompactInject_DuringAutonomousTurn_WaitsForIdle pins #2147: a /compact
// that arrives while CC runs a turn foci did not start must not be handed to
// CC's queue (CC >= 2.1.287 runs a queued slash command when the turn ends).
// The idle that ends the running turn used to resolve the armed compaction
// wait as "no boundary" before the queued /compact ever ran, so the user saw
// "Nothing to compact" and the later /compact run was adopted as autonomous.
// The inject now waits for that idle, then claims its own turn and sends.
func TestCompactInject_DuringAutonomousTurn_WaitsForIdle(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}

	var opens int
	b.SetOnAutonomousOpen(func() {
		opens++
		b.AdoptRunningTurn(&delegator.TurnEvents{TurnID: "auto"})
	})
	stateEvent(b, "running") // CC starts an autonomous run; foci adopts it
	if opens != 1 || !b.IsTurnInFlight() {
		t.Fatalf("setup: opens=%d in_flight=%v, want an adopted turn", opens, b.IsTurnInFlight())
	}

	b.ArmCompactionWait()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- b.ImmediateInject(ctx, delegator.Inject{Source: delegator.SourceCompact, Text: "/compact x"})
	}()

	// Sync on the inject having parked behind the running turn before ending
	// that turn; without this the goroutine could start after the idle and
	// take the idle path, passing on the old code too.
	for {
		b.turnMu.Lock()
		parked := b.compactDeferred
		b.turnMu.Unlock()
		if parked {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("ImmediateInject returned (%v) with a turn in flight; want it to wait for the turn to end", err)
		case <-ctx.Done():
			t.Fatal("ImmediateInject never parked behind the running turn")
		case <-time.After(time.Millisecond):
		}
	}

	// The autonomous run ends. The /compact has not run yet, so this idle
	// says nothing about it.
	stateEvent(b, "idle")
	if err := <-done; err != nil {
		t.Fatalf("ImmediateInject(SourceCompact): %v", err)
	}
	if !strings.Contains(buf.String(), "/compact x") {
		t.Fatalf("/compact not sent after the running turn ended; wrote %q", buf.String())
	}
	if !b.IsTurnInFlight() {
		t.Fatal("/compact must claim its own turn so its run is not adopted as autonomous")
	}

	// The /compact run itself: running (not adopted), boundary, idle.
	stateEvent(b, "running")
	if opens != 1 {
		t.Fatalf("/compact run was adopted as autonomous; opens=%d", opens)
	}
	braw, _ := json.Marshal(CompactBoundaryMessage{CompactMetadata: CompactMetadata{PreTokens: 1000}})
	b.OnSystem("compact_boundary", braw)
	stateEvent(b, "idle")

	if err := b.WaitForCompaction(ctx); err != nil {
		t.Fatalf("WaitForCompaction = %v, want nil (the compaction happened)", err)
	}
}
