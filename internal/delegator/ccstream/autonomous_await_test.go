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

	t.Run("live autonomous run holds via in-flight", func(t *testing.T) {
		var buf bytes.Buffer
		b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
		b.typingFunc = func(bool) {}
		// A live autonomous run is now a first-class turn (turnActive) — held by
		// the normal in-flight gate, not AwaitingAutonomousRun (which covers only
		// pending/grace).
		b.SetOnAutonomousOpen(func() { b.AdoptRunningTurn(&delegator.TurnEvents{}) })
		stateEvent(b, "running") // no foci turn open → adopted as a first-class turn
		if !b.IsTurnInFlight() {
			t.Fatal("an adopted autonomous run must be in flight (turnActive)")
		}
	})

	t.Run("post-run grace holds then releases", func(t *testing.T) {
		var buf bytes.Buffer
		b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
		b.typingFunc = func(bool) {}
		b.SetOnAutonomousOpen(func() { b.AdoptRunningTurn(&delegator.TurnEvents{}) })
		stateEvent(b, "running")
		stateEvent(b, "idle") // completeTurn stamps lastAutonomousEnd (turnAutonomous) → grace open
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

// TestTaskStopClearsViaNotification: a TaskStop'd task leaves the tracker on the
// task_notification status "stopped" that CC sends for it (verified live on CC
// 2.1.280, #2022), and ONLY there. The TaskStop tool_use used to RemoveOne itself,
// on the belief that no notification follows a stop; with the notification now
// handled, that would retire a second, unrelated entry: stopping the NEWER of two
// commands removed the older one at the tool_use and the stopped one at the
// notification, leaving nothing tracked while the older command still ran.
func TestTaskStopClearsViaNotification(t *testing.T) {
	t.Parallel()

	mkTool := func(id, name, input string) *AssistantMessage {
		return &AssistantMessage{Message: BetaMessage{
			Content: []ContentBlock{{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)}},
		}}
	}

	b := &Backend{}
	applyHandler(b, &testHandler{})

	b.OnAssistant(mkTool("bg1", "Bash", `{"command":"sleep 60","run_in_background":true}`))
	b.OnAssistant(mkTool("bg2", "Bash", `{"command":"sleep 60","run_in_background":true}`))
	if n := b.agents.Pending(); n != 2 {
		t.Fatalf("setup: Pending() = %d, want 2", n)
	}

	// Stop the newer one. The stream order is the TaskStop tool_use, then the
	// notification naming the stopped task's own tool_use_id.
	b.OnAssistant(mkTool("stop1", "TaskStop", `{"task_id":"bg2abcd"}`))
	raw, _ := json.Marshal(TaskEvent{
		Subtype: "task_notification", Status: "stopped", TaskID: "bg2abcd", ToolUseID: "bg2",
	})
	b.OnSystem("task_notification", raw)

	if n := b.agents.Pending(); n != 1 {
		t.Fatalf("after stopping one of two: Pending() = %d, want 1 (bg1 still running)", n)
	}
	if b.agents.Remove("bg2") {
		t.Fatal("the stopped entry bg2 is still tracked")
	}
	if !b.agents.Remove("bg1") {
		t.Fatal("the still-running entry bg1 was retired by the stop of bg2")
	}
}

// TestTaskNotificationTerminalStatuses: every terminal task_notification status
// ends the task, not only "completed" (#2022). A background Bash exiting non-zero
// arrives as "failed" and a stopped task as "stopped"; handling only "completed"
// left both tracked, and their chits running, until the max-age prune. A
// non-terminal status must still change nothing. Pinned on an Agent run, the
// kind that has a chit to end; a Bash's tracker removal on "failed" is pinned by
// TestTaskNotification_NoSubagentEndForBash.
func TestTaskNotificationTerminalStatuses(t *testing.T) {
	t.Parallel()

	mkAgent := func(id string) *AssistantMessage {
		return &AssistantMessage{Message: BetaMessage{
			Content: []ContentBlock{{Type: "tool_use", ID: id, Name: "Agent",
				Input: json.RawMessage(`{"description":"researcher","prompt":"go"}`)}},
		}}
	}

	for _, tc := range []struct {
		status string
		ends   bool
	}{
		{"completed", true},
		{"failed", true},
		{"stopped", true},
		{"running", false},
		{"", false},
	} {
		t.Run("status="+tc.status, func(t *testing.T) {
			t.Parallel()
			var ended []string
			b := &Backend{hookInstallID: "install-a"}
			applyHandler(b, &testHandler{
				OnSubagentStart: func(string, string, string, int) {},
				OnSubagentEnd:   func(groupKey string, _ int) { ended = append(ended, groupKey) },
			})

			b.OnAssistant(mkAgent("toolu_agent"))
			fireAgentPreToolUse(b, "toolu_agent", "install-a", `{"description":"researcher","prompt":"go"}`)
			raw, _ := json.Marshal(TaskEvent{
				Subtype: "task_notification", Status: tc.status, TaskID: "axyz", ToolUseID: "toolu_agent",
			})
			b.OnSystem("task_notification", raw)

			wantPending, wantEnded := 1, 0
			if tc.ends {
				wantPending, wantEnded = 0, 1
			}
			if n := b.agents.Pending(); n != wantPending {
				t.Errorf("Pending() = %d, want %d", n, wantPending)
			}
			if len(ended) != wantEnded {
				t.Errorf("OnSubagentEnd fired %d times (%v), want %d", len(ended), ended, wantEnded)
			}
		})
	}
}

// TestTaskNotificationRemovesNamedEntry: a completion must clear the entry it
// NAMES, not simply the oldest one (#1770).
//
// The tracker's removal was count-based (RemoveOne drops pending[0]) on the
// grounds that the gate only reads a COUNT, so which entry goes does not
// matter. It does. The count is only self-correcting while every Add is
// balanced by exactly one completion — and the max-age prune breaks that by
// design, dropping an entry whose completion is still to come.
func TestTaskNotificationRemovesNamedEntry(t *testing.T) {
	t.Parallel()

	mkTool := func(id, name, input string) *AssistantMessage {
		return &AssistantMessage{Message: BetaMessage{
			Content: []ContentBlock{{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)}},
		}}
	}
	complete := func(b *Backend, toolUseID string) {
		raw, _ := json.Marshal(TaskEvent{
			Subtype: "task_notification", Status: "completed", ToolUseID: toolUseID,
		})
		b.OnSystem("task_notification", raw)
	}

	// The entry named by the notification is the one that goes, even when it is
	// not the oldest. Distinguishable only by description: a count-based removal
	// keeps Pending() correct here while silently retiring the wrong subagent,
	// so the status detail is what exposes it.
	t.Run("removes the named entry, not the oldest", func(t *testing.T) {
		t.Parallel()
		b := &Backend{}
		applyHandler(b, &testHandler{})
		var status string
		b.agents.OnStatus = func(s string) { status = s }

		b.OnAssistant(mkTool("toolu_agent", "Agent", `{"description":"researcher","prompt":"go"}`))
		b.OnAssistant(mkTool("toolu_bash", "Bash", `{"command":"sleep 60","run_in_background":true}`))
		if n := b.agents.Pending(); n != 2 {
			t.Fatalf("setup: Pending() = %d, want 2", n)
		}

		complete(b, "toolu_bash") // the SECOND entry finishes first
		if n := b.agents.Pending(); n != 1 {
			t.Fatalf("after completion: Pending() = %d, want 1", n)
		}
		if status != "researcher" {
			t.Fatalf("removed the wrong entry: surviving detail = %q, want \"researcher\"", status)
		}
	})

	// The production failure. An entry the tracker no longer holds — aged out by
	// the max-age prune while its job was still running, or never detected —
	// completes late. A count-based removal then retires an UNRELATED pending
	// entry, releasing the spec-§4 inject gate for work that is still in flight.
	// Observed 2026-08-21: two ~45-minute background commands against a 30-minute
	// backstop, one pruned at 39m46s.
	t.Run("unknown id does not retire an unrelated entry", func(t *testing.T) {
		t.Parallel()
		b := &Backend{}
		applyHandler(b, &testHandler{})

		b.OnAssistant(mkTool("toolu_still_running", "Bash", `{"command":"sleep 3600","run_in_background":true}`))
		if n := b.agents.Pending(); n != 1 {
			t.Fatalf("setup: Pending() = %d, want 1", n)
		}

		complete(b, "toolu_already_pruned")
		if n := b.agents.Pending(); n != 1 {
			t.Fatalf("a stranger's completion retired a live entry: Pending() = %d, want 1", n)
		}
	})
}

// TestTaskNotification_NoSubagentEndForBash pins #2010: a background Bash task
// sends NEITHER a SubagentStart NOR a SubagentEnd, whether the main thread ran
// it or a subagent did (CC auto-backgrounds a subagent's long Bash). CC sends
// task_started/task_notification for both, and the notification used to fire
// OnSubagentEnd for a group that was never opened — 837 of 844 orphan
// subagent.end frames in the 7 days to 2026-09-25. The tracker entry must still
// be retired, or the spec-§4 inject gate stays held until the max-age prune.
// The Agent case is the control: its end must still go out.
func TestTaskNotification_NoSubagentEndForBash(t *testing.T) {
	t.Parallel()

	mkTool := func(id, name, input string) *AssistantMessage {
		return &AssistantMessage{Message: BetaMessage{
			Content: []ContentBlock{{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)}},
		}}
	}
	// The shapes below are the ones captured live from CC 2.1.280 (#1554 probe,
	// /tmp/cc-nestbg.zRjuKf): task_notification carries no task_type.
	started := func(taskID, toolUseID, taskType string, ownedBySubagent bool) []byte {
		raw, _ := json.Marshal(map[string]any{
			"type": "system", "subtype": "task_started", "task_id": taskID, "tool_use_id": toolUseID,
			"task_type": taskType, "owned_by_subagent": ownedBySubagent,
		})
		return raw
	}
	notified := func(taskID, toolUseID, status string) []byte {
		raw, _ := json.Marshal(map[string]any{
			"type": "system", "subtype": "task_notification", "task_id": taskID, "tool_use_id": toolUseID,
			"status": status,
		})
		return raw
	}

	for _, tc := range []struct {
		name      string
		setup     func(b *Backend)
		taskID    string
		toolUseID string
		taskType  string
		ownedBySA bool
		status    string
		wantEnd   bool
	}{
		{
			name: "top-level run_in_background Bash",
			setup: func(b *Backend) {
				b.OnAssistant(mkTool("toolu_bash", "Bash", `{"command":"sleep 60","run_in_background":true}`))
			},
			taskID: "bxyz", toolUseID: "toolu_bash", taskType: taskTypeBash, status: "completed",
		},
		{
			name: "top-level Bash that failed",
			setup: func(b *Backend) {
				b.OnAssistant(mkTool("toolu_bash", "Bash", `{"command":"exit 3","run_in_background":true}`))
			},
			taskID: "bxyz", toolUseID: "toolu_bash", taskType: taskTypeBash, status: "failed",
		},
		{
			// The subagent's Bash tool_use never reaches the main thread as a
			// top-level block, so nothing was tracked for it.
			name:   "subagent-owned auto-backgrounded Bash",
			setup:  func(*Backend) {},
			taskID: "bzqu24t0r", toolUseID: "toolu_subbash", taskType: taskTypeBash, ownedBySA: true, status: "completed",
		},
		{
			name: "Agent (control)",
			setup: func(b *Backend) {
				b.OnAssistant(mkTool("toolu_agent", "Agent", `{"description":"researcher","prompt":"go"}`))
			},
			taskID: "a19604ed24b221564", toolUseID: "toolu_agent", taskType: "local_agent", status: "completed",
			wantEnd: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var starts, ends []string
			b := &Backend{}
			applyHandler(b, &testHandler{
				OnSubagentStart: func(groupKey, _, _ string, _ int) { starts = append(starts, groupKey) },
				OnSubagentEnd:   func(groupKey string, _ int) { ends = append(ends, groupKey) },
			})

			tc.setup(b)
			b.OnSystem("task_started", started(tc.taskID, tc.toolUseID, tc.taskType, tc.ownedBySA))
			b.OnSystem("task_notification", notified(tc.taskID, tc.toolUseID, tc.status))

			if n := b.agents.Pending(); n != 0 {
				t.Errorf("Pending() = %d after the task ended, want 0 (tracker entry not retired)", n)
			}
			wantN := 0
			if tc.wantEnd {
				wantN = 1
			}
			if len(starts) != wantN {
				t.Errorf("OnSubagentStart fired %d times (%v), want %d", len(starts), starts, wantN)
			}
			if len(ends) != wantN {
				t.Errorf("OnSubagentEnd fired %d times (%v), want %d", len(ends), ends, wantN)
			}
		})
	}
}
