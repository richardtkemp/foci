package ccstream

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"foci/internal/delegator"
)

// The ccstream backend satisfies the optional AutonomousRunAwaiter interface the
// inbox uses to gate system injects across the background-work window (spec §4).
var _ delegator.AutonomousRunAwaiter = (*Backend)(nil)

// TestAwaitingAutonomousRun covers the three dimensions of the pending-work gate:
// pending background work, a live autonomous run, and the post-run grace — plus
// the quiescent case where none hold.
func TestAwaitingAutonomousRun(t *testing.T) {
	t.Parallel()

	t.Run("quiescent is false", func(t *testing.T) {
		var buf bytes.Buffer
		b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
		if b.AwaitingAutonomousRun() {
			t.Fatal("fresh backend must not be awaiting")
		}
	})

	t.Run("pending subagent holds", func(t *testing.T) {
		var buf bytes.Buffer
		b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
		b.agents.Add("sub1", "explore")
		if !b.AwaitingAutonomousRun() {
			t.Fatal("a pending subagent must hold the gate")
		}
		b.agents.RemoveOne()
		if b.AwaitingAutonomousRun() {
			t.Fatal("gate must release once the subagent completes")
		}
	})

	t.Run("live autonomous run holds", func(t *testing.T) {
		var buf bytes.Buffer
		b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
		b.typingFunc = func(bool) {}
		stateEvent(b, "running") // no foci turn open → autonomous run
		if !b.AwaitingAutonomousRun() {
			t.Fatal("a live autonomous run must hold the gate")
		}
	})

	t.Run("post-run grace holds then releases", func(t *testing.T) {
		var buf bytes.Buffer
		b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
		b.typingFunc = func(bool) {}
		stateEvent(b, "running")
		stateEvent(b, "idle") // ends the run, stamps lastAutonomousEnd → grace open
		if !b.AwaitingAutonomousRun() {
			t.Fatal("within the post-run grace the gate must still hold")
		}
		// Push the run's end beyond the grace window.
		b.turnMu.Lock()
		b.lastAutonomousEnd = time.Now().Add(-2 * autonomousInjectGrace)
		b.turnMu.Unlock()
		if b.AwaitingAutonomousRun() {
			t.Fatal("past the grace window the gate must release")
		}
	})
}

// TestTryBeginTurn_RejectsWhilePending pins the SourceSystem-path rejection: a
// system turn cannot begin while background work is pending (its completion will
// chain an autonomous run that owns delivery — spec §4). Once the work clears,
// the turn begins.
func TestTryBeginTurn_RejectsWhilePending(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}

	b.agents.Add("sub1", "explore")
	if err := b.tryBeginTurn(&delegator.TurnEvents{}); err != delegator.ErrTurnInFlight {
		t.Fatalf("tryBeginTurn while pending = %v, want ErrTurnInFlight", err)
	}

	b.agents.RemoveOne()
	if err := b.tryBeginTurn(&delegator.TurnEvents{}); err != nil {
		t.Fatalf("tryBeginTurn after pending cleared = %v, want nil", err)
	}
}

// TestBashBackgroundTracked verifies a run_in_background Bash tool_use is added
// to the tracker (so it counts toward Pending()/the gate) while a synchronous
// Bash is not.
func TestBashBackgroundTracked(t *testing.T) {
	t.Parallel()

	mkBash := func(id, input string) *AssistantMessage {
		return &AssistantMessage{Message: BetaMessage{
			Content: []ContentBlock{{Type: "tool_use", ID: id, Name: "Bash", Input: json.RawMessage(input)}},
		}}
	}

	b := &Backend{}
	applyHandler(b, &testHandler{})

	b.OnAssistant(mkBash("fg", `{"command":"ls"}`))
	if n := b.agents.Pending(); n != 0 {
		t.Fatalf("foreground Bash tracked: Pending() = %d, want 0", n)
	}

	b.OnAssistant(mkBash("bg", `{"command":"sleep 60","run_in_background":true}`))
	if n := b.agents.Pending(); n != 1 {
		t.Fatalf("backgrounded Bash not tracked: Pending() = %d, want 1", n)
	}
}

// TestTaskStopClearsPending verifies a TaskStop tool_use decrements the tracker.
// A stopped background task never emits a task_notification, so without this the
// entry would linger in Pending() until the 30-min prune, holding the gate.
func TestTaskStopClearsPending(t *testing.T) {
	t.Parallel()

	mkTool := func(id, name, input string) *AssistantMessage {
		return &AssistantMessage{Message: BetaMessage{
			Content: []ContentBlock{{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)}},
		}}
	}

	b := &Backend{}
	applyHandler(b, &testHandler{})

	// Two background commands → Pending() == 2.
	b.OnAssistant(mkTool("bg1", "Bash", `{"command":"sleep 60","run_in_background":true}`))
	b.OnAssistant(mkTool("bg2", "Bash", `{"command":"sleep 60","run_in_background":true}`))
	if n := b.agents.Pending(); n != 2 {
		t.Fatalf("setup: Pending() = %d, want 2", n)
	}

	// A TaskStop decrements one.
	b.OnAssistant(mkTool("stop1", "TaskStop", `{"task_id":"bg1abcd"}`))
	if n := b.agents.Pending(); n != 1 {
		t.Fatalf("after one TaskStop: Pending() = %d, want 1", n)
	}

	// A second TaskStop clears the last.
	b.OnAssistant(mkTool("stop2", "TaskStop", `{"task_id":"bg2abcd"}`))
	if n := b.agents.Pending(); n != 0 {
		t.Fatalf("after two TaskStops: Pending() = %d, want 0", n)
	}

	// A TaskStop against an empty tracker is a safe no-op (stays 0, no panic).
	b.OnAssistant(mkTool("stop3", "TaskStop", `{"task_id":"gone"}`))
	if n := b.agents.Pending(); n != 0 {
		t.Fatalf("TaskStop on empty tracker: Pending() = %d, want 0", n)
	}
}
