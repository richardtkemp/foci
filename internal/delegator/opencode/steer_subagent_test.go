package opencode

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"foci/internal/delegator"
)

// #2753: opencode's POST /abort cascades to every running child (Task tool)
// session, so a mid-turn steer that aborts while subagents run returns
// "Task cancelled" to the parent and throws their work away. A steer must
// wait until the last running subagent has ended, then abort (so the steer
// still lands right after the results, as it does on CC).

func startTask(b *Backend, callID string) {
	in, _ := json.Marshal(map[string]string{"description": callID, "prompt": "p"})
	b.handleToolPart(Part{CallID: callID, Tool: taskTool, State: &ToolState{Status: ToolStateRunning, Input: in}})
}

func endTask(b *Backend, callID string, status string) {
	st := &ToolState{Status: status, Output: `"done"`}
	if status == ToolStateError {
		st.Error = "boom"
	}
	b.handleToolPart(Part{CallID: callID, Tool: taskTool, State: st})
}

func TestInject_Steer_WhileSubagentsRun_DefersAbortUntilLastEnds(t *testing.T) {
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)
	b.abortDrainTimeout = time.Hour

	b.beginTurn(&delegator.TurnEvents{})
	startTask(b, "task-1")
	startTask(b, "task-2")

	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer, Text: "how did you start the agents?",
	}); err != nil {
		t.Fatalf("Inject steer: %v", err)
	}
	if got := rec.countPath("/session/sess-inject/abort"); got != 0 {
		t.Fatalf("/abort POSTs while 2 subagents run = %d, want 0 (abort would cancel them)", got)
	}

	// A second steer while still waiting just appends.
	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer, Text: "and why?",
	}); err != nil {
		t.Fatalf("Inject second steer: %v", err)
	}

	endTask(b, "task-1", ToolStateCompleted)
	if got := rec.countPath("/session/sess-inject/abort"); got != 0 {
		t.Fatalf("/abort POSTs with 1 subagent still running = %d, want 0", got)
	}

	// The last subagent ends (an error end counts too): now the steer aborts.
	endTask(b, "task-2", ToolStateError)
	if got := rec.countPath("/session/sess-inject/abort"); got != 1 {
		t.Fatalf("/abort POSTs after last subagent ended = %d, want 1", got)
	}

	// The normal abort drain then flushes both steers as one turn.
	b.onSessionError(b.sessionID, &MessageError{Name: ErrMessageAborted})
	b.onSessionIdle(b.sessionID)
	b.onSessionIdle(b.sessionID)
	if got := rec.countPath("/session/sess-inject/prompt_async"); got != 1 {
		t.Fatalf("/prompt_async after drain = %d, want 1", got)
	}
	req, _ := rec.lastPath("/session/sess-inject/prompt_async")
	var body struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := "how did you start the agents?\n\nand why?"; len(body.Parts) != 1 || body.Parts[0].Text != want {
		t.Errorf("flushed text = %+v, want %q", body.Parts, want)
	}
	b.turnMu.Lock()
	deferred := b.steerAbortDeferred
	b.turnMu.Unlock()
	if deferred {
		t.Error("steerAbortDeferred still set after the abort fired")
	}
}

func TestInject_Steer_WhileSubagentsRun_TurnEndsFirst_FlushesWithoutAbort(t *testing.T) {
	// If the turn reaches idle on its own while the steer waits (e.g. the
	// tracker never saw the task end), the steer flushes at that idle as a
	// follow-up turn and no abort is ever sent.
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)
	b.abortDrainTimeout = time.Hour

	b.beginTurn(&delegator.TurnEvents{})
	startTask(b, "task-1")
	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer, Text: "steer",
	}); err != nil {
		t.Fatalf("Inject steer: %v", err)
	}
	b.onSessionIdle(b.sessionID)

	if got := rec.countPath("/session/sess-inject/abort"); got != 0 {
		t.Errorf("/abort POSTs = %d, want 0", got)
	}
	if got := rec.countPath("/session/sess-inject/prompt_async"); got != 1 {
		t.Errorf("/prompt_async = %d, want 1 (flushed at natural idle)", got)
	}
	b.turnMu.Lock()
	deferred := b.steerAbortDeferred
	b.turnMu.Unlock()
	if deferred {
		t.Error("steerAbortDeferred still set after the turn ended")
	}

	// A task ending later (stale event) must not abort the new turn.
	endTask(b, "task-1", ToolStateCompleted)
	if got := rec.countPath("/session/sess-inject/abort"); got != 0 {
		t.Errorf("/abort POSTs after a late task end = %d, want 0", got)
	}
}

func TestInject_Steer_NoSubagents_StillAbortsAtOnce(t *testing.T) {
	// Control: without running subagents the steer aborts immediately, as before.
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)
	b.abortDrainTimeout = time.Hour

	b.beginTurn(&delegator.TurnEvents{})
	startTask(b, "task-1")
	endTask(b, "task-1", ToolStateCompleted)
	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer, Text: "steer",
	}); err != nil {
		t.Fatalf("Inject steer: %v", err)
	}
	if got := rec.countPath("/session/sess-inject/abort"); got != 1 {
		t.Errorf("/abort POSTs = %d, want 1", got)
	}
}
