package codex

import (
	"testing"
	"time"

	"foci/internal/delegator"
)

// A codex subagent runs in its OWN codex thread, and the app-server streams
// that thread's full lifecycle down the SAME connection as the parent's:
//
//	thread/status/changed  idle -> active
//	turn/started           inProgress          (threadId = the CHILD's)
//	item/agentMessage/delta x N
//	item/completed         agentMessage
//	thread/status/changed  -> idle
//	turn/completed         completed
//
// Verified live on codex 0.144.5 AND 0.145.0, for BOTH the subagents feature
// and collab mode. foci registers only its own sessions' threads, so a child
// thread id resolves to no facade and dispatch() falls through to the process
// OWNER — meaning, while a parent waits on its subagent, the child's
// turn/completed completes the PARENT's live turn, the child's text streams
// into the user's chat as the parent's, and the child's token usage overwrites
// the parent's. Same class as the batch-thread leak fixed in 825ac551, which
// subagent threads never got a guard for.
func TestSubagentChildThreadDoesNotDisturbTheOwner(t *testing.T) {
	b := newTestBackend(t)
	b.subagents = newSubagentTracker()

	const child = "child-thread-1"
	var completed *delegator.TurnResult
	var ownerText []string
	b.turnMu.Lock()
	b.turnActive = true
	b.turnEvents = &delegator.TurnEvents{OnTurnComplete: func(r *delegator.TurnResult) { completed = r }}
	b.turnMu.Unlock()
	ev := captureSubagentEvents(b)
	se := b.sessionEvents.Load()
	se.OnText = func(s string) { ownerText = append(ownerText, s) }
	b.sessionEvents.Store(se)

	// The parent spawns a child; foci opens a run for it.
	b.onItemCompleted(&itemCompletedParams{Item: activityItem("call_1", "started", child)})
	if len(ev.starts) != 1 {
		t.Fatalf("expected the run to open, got starts=%+v", ev.starts)
	}

	// Everything below carries the CHILD's threadId.
	b.dispatch([]byte(`{"method":"turn/started","params":{"threadId":"` + child + `","turn":{"id":"t1","status":"inProgress"}}}`))
	b.dispatch([]byte(`{"method":"item/agentMessage/delta","params":{"threadId":"` + child + `","itemId":"m1","delta":"Earth hides golden moons"}}`))
	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"` + child + `","item":{"type":"agentMessage","id":"m1","text":"Earth hides golden moons","phase":"final_answer"}}}`))
	b.dispatch([]byte(`{"method":"thread/tokenUsage/updated","params":{"threadId":"` + child + `","tokenUsage":{"last":{"inputTokens":16856,"outputTokens":187,"totalTokens":17043}}}}`))
	b.dispatch([]byte(`{"method":"turn/completed","params":{"threadId":"` + child + `","turn":{"id":"t1","status":"completed"}}}`))

	// The owner's turn must be untouched.
	if completed != nil {
		t.Errorf("the child's turn/completed COMPLETED THE PARENT'S TURN (result=%+v) — the parent is still working and its turn just ended", completed)
	}
	b.turnMu.Lock()
	active, text, usage := b.turnActive, b.turnText.String(), b.stashedUsage
	b.turnMu.Unlock()
	if !active {
		t.Error("parent turnActive = false — the child ended the parent's turn")
	}
	if text != "" {
		t.Errorf("parent turnText = %q — the child's output was accumulated as the parent's answer", text)
	}
	if usage != nil {
		t.Errorf("parent stashedUsage = %+v — the child's token usage overwrote the parent's", usage)
	}
	if len(ownerText) != 0 {
		t.Errorf("child text streamed into the user's chat as the parent's: %v", ownerText)
	}

	// And the child's own turn/completed is what ends its run — the signal
	// afe20cd0 concluded did not exist because SubAgentActivityKind has no
	// completion variant. The enum has none; the child's THREAD does.
	if len(ev.ends) != 1 {
		t.Errorf("ends = %+v, want 1 — the child's turn/completed is the completion signal", ev.ends)
	}
	if len(ev.texts) != 1 || ev.texts[0].prompt != "Earth hides golden moons" {
		t.Errorf("subagent texts = %+v, want the child's message delivered to the subagent panel", ev.texts)
	}
}

// finishAll must no longer be the terminator: a child that outlives its
// parent's turn (proven live — turn/completed arrived with the child still
// working) keeps its run open until the child's own turn ends.
func TestParentTurnCompletionDoesNotEndALiveChild(t *testing.T) {
	b := newTestBackend(t)
	b.subagents = newSubagentTracker()
	ev := captureSubagentEvents(b)

	const child = "child-thread-2"
	b.onItemCompleted(&itemCompletedParams{Item: activityItem("call_1", "started", child)})
	b.dispatch([]byte(`{"method":"turn/started","params":{"threadId":"` + child + `","turn":{"id":"t1","status":"inProgress"}}}`))

	// The PARENT's turn ends while the child is still working.
	b.dispatch([]byte(`{"method":"turn/completed","params":{"threadId":"","turn":{"id":"p1","status":"completed"}}}`))
	if len(ev.ends) != 0 {
		t.Errorf("ends = %+v after the PARENT's turn completed — the child is still working (#1588)", ev.ends)
	}

	// The child finishing is what ends it.
	b.dispatch([]byte(`{"method":"turn/completed","params":{"threadId":"` + child + `","turn":{"id":"t1","status":"completed"}}}`))
	waitUntil(t, 2*time.Second, func() bool { return len(ev.ends) == 1 },
		"the child's own turn/completed did not end its run")
}

func waitUntil(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}
