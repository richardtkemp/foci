package app

import (
	"encoding/json"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/delegator/ccstream"
)

// TestSubagentEnd_WithFiredWake_DoesNotDeadlock reproduces the 18 Nov 2026
// work Mac hang (#3169) end to end, with the real ccstream backend, app hub
// and reminder store. The session has a fired wake, so its reminder state
// asks whether its input is held behind background work; that question goes
// to the backend's subagent tracker. When a background task ends, the tracker
// reports the new running list to the app, whose waiting-on-other refresh
// re-reads the wake state, and so the tracker, from inside the report. The
// tracker held its (non-re-entrant) lock while it reported: the stream
// reader goroutine deadlocked on itself (goroutine 38590 in the dump).
//
// The held predicate stands in for Agent.SystemInputHeld →
// DelegatedManager.BackendAwaitingAutonomousRun, which call the backend's
// AwaitingAutonomousRun and take no lock of their own on that path; the two
// callbacks stand in for the gateway's SubagentRunningFunc and
// SubagentStatusFunc, which call SetSubagents and SetSubagentDetail directly.
func TestSubagentEnd_WithFiredWake_DoesNotDeadlock(t *testing.T) {
	h := newTestHub()
	be := &ccstream.Backend{}
	rs := withReminderStore(t, h, "ag", func(string) bool { return be.AwaitingAutonomousRun() })

	b := h.ensureBinding(nil, "ag", "conv-a")
	c := fakeClient()
	b.attach(c)
	sk := b.key()
	be.SetOnSubagentRunning(func(r []delegator.RunningSubagent) { SetSubagents(sk, r) })
	be.SetOnSubagentStatus(func(d string) { SetSubagentDetail(sk, d) })

	// A background command starts: the conversation shows it running.
	be.OnAssistant(&ccstream.AssistantMessage{Message: ccstream.BetaMessage{
		Content: []ccstream.ContentBlock{{Type: "tool_use", ID: "bg1", Name: "Bash",
			Input: json.RawMessage(`{"command":"sleep 60","description":"long build","run_in_background":true}`)}},
	}})
	b.mu.Lock()
	running := len(b.subagents)
	b.mu.Unlock()
	if running != 1 {
		t.Fatalf("setup: conversation lists %d running entries, want 1", running)
	}

	// The session's wake fires and waits behind the background command.
	id, err := rs.AddWake("ag", sk, "check the build", "30m")
	if err != nil {
		t.Fatal(err)
	}
	rs.MarkWakeFired(id, sk)

	// The command ends.
	raw, _ := json.Marshal(ccstream.TaskEvent{Subtype: "task_notification", Status: "completed", TaskID: "bg1abcd", ToolUseID: "bg1"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		be.OnSystem("task_notification", raw)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("task_notification did not return: the app's waiting-on-other refresh re-entered the subagent tracker from its own callback and deadlocked")
	}

	b.mu.Lock()
	running = len(b.subagents)
	detail := b.subagentDetail
	b.mu.Unlock()
	if running != 0 || detail != "" {
		t.Errorf("after the task ended: %d running entries, detail %q; want none", running, detail)
	}
}

// TestWaitingRefresh_TrackerPruneDoesNotReenter is the second deadlock on the
// #3169 path, found in its audit. refreshWaiting holds the conversation's
// waitMu while it asks whether the session's input is held, and that question
// reads the tracker's Pending, which prunes an entry past its max age. Had the
// prune's status report run on the caller's goroutine (as it did, under the
// tracker lock, before #3169), it would reach this conversation's
// applyActivity hook, refreshWaiting, and block on the waitMu its own caller
// holds. A tracker read therefore never runs a callback on its caller's
// goroutine; the prune is still reported, just afterwards.
func TestWaitingRefresh_TrackerPruneDoesNotReenter(t *testing.T) {
	h := newTestHub()
	tr := &delegator.SubagentTracker{MaxAge: time.Nanosecond}
	rs := withReminderStore(t, h, "ag", func(string) bool { return tr.Pending() > 0 })

	b := h.ensureBinding(nil, "ag", "conv-a")
	c := fakeClient()
	b.attach(c)
	sk := b.key()
	tr.OnRunning = func(r []delegator.RunningSubagent) { SetSubagents(sk, r) }

	// A background command, not an Agent subagent: a command does not make the
	// conversation busy outright, so the refresh goes on to the wake list and
	// the held predicate. It is stale at once under the 1ns max age.
	tr.AddEntry(delegator.TrackedSubagent{ID: "bg1", Description: "long build", Kind: delegator.SubagentKindCommand})
	b.mu.Lock()
	running := len(b.subagents)
	b.mu.Unlock()
	if running != 1 {
		t.Fatalf("setup: conversation lists %d running entries, want 1", running)
	}

	// A fired wake, so the refresh asks the held predicate. Detach the store's
	// change hook first: its push would read the tracker (and prune) outside
	// waitMu, and this test needs the prune to happen inside it.
	rs.SetOnChange(nil)
	id, err := rs.AddWake("ag", sk, "check the build", "30m")
	if err != nil {
		t.Fatal(err)
	}
	rs.MarkWakeFired(id, sk)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.refreshWaiting(b)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("refreshWaiting did not return: the tracker's prune report re-entered refreshWaiting for the same conversation")
	}

	// The prune still reaches the conversation.
	deadline := time.Now().Add(10 * time.Second)
	for {
		b.mu.Lock()
		running = len(b.subagents)
		b.mu.Unlock()
		if running == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pruned entry was never reported to the conversation")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
