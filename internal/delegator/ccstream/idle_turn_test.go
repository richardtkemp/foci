package ccstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
)

// ---------------------------------------------------------------------------
// Idle-keyed turn completion (#813 successor)
// ---------------------------------------------------------------------------
//
// The turn boundary is CC's session_state_changed running/idle SDK stream,
// not `result` events: CC mints 0, 1 or N results per logical turn (a "now"
// steer aborts the current ask and adds one; a steer landing mid-tool folds
// and adds none; results are withheld while background agents run), while
// `idle` fires exactly once per run, after the last result — probe-verified
// against the deployed CC (clutch docs steer-shadow-turn-design-option3.md,
// Phase 3). These tests replay the production event shapes from the
// 2026-07-02 incidents and pin the lifecycle: stash on result, complete on
// idle, legacy fallback when state events are absent.

// stateEvent feeds a session_state_changed system message through OnSystem,
// the same path the reader dispatches.
func stateEvent(b *Backend, state string) {
	raw, _ := json.Marshal(map[string]string{
		"type": "system", "subtype": "session_state_changed", "state": state,
	})
	b.OnSystem("session_state_changed", raw)
}

// errWriteCloser fails every write — for exercising send-failure fallbacks.
type errWriteCloser struct{}

func (errWriteCloser) Write([]byte) (int, error) { return 0, errors.New("stdin closed") }
func (errWriteCloser) Close() error              { return nil }

// TestIdleKeyed_CompletesOnIdleNotResult proves the core lifecycle: a result
// stashes but does NOT complete the turn (the turn stays in flight so a
// colliding inject cannot begin a rogue turn), and the following idle
// completes it exactly once with the stashed content.
func TestIdleKeyed_CompletesOnIdleNotResult(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}

	var completed []*delegator.TurnResult
	handler := &testHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completed = append(completed, r) },
	}
	applyHandler(b, handler)
	stateEvent(b, "running")

	b.turnMu.Lock()
	b.turnText.WriteString("the reply")
	b.turnMu.Unlock()
	b.OnResult(&ResultMessage{Subtype: "success", ModelUsage: map[string]ModelUsage{}})

	if len(completed) != 0 {
		t.Fatalf("result must not complete the turn; OnTurnComplete fired %d times", len(completed))
	}
	if !b.IsTurnInFlight() {
		t.Fatal("turn must stay in flight between result and idle")
	}

	stateEvent(b, "idle")

	if len(completed) != 1 {
		t.Fatalf("idle should complete the turn exactly once; got %d", len(completed))
	}
	if completed[0].Text != "the reply" {
		t.Errorf("completed with wrong text: %q", completed[0].Text)
	}
	if b.IsTurnInFlight() {
		t.Error("turn should be released after idle")
	}
}

// TestIdleKeyed_SteerAbortCycle replays the abort shape (steer arrives
// mid-stream → CC aborts the ask, mints an early result, answers in a second
// ask cycle): both results stash into ONE turn, output tokens sum across the
// cycles, and the single completion at idle carries the full accumulated text.
func TestIdleKeyed_SteerAbortCycle(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}

	var completed []*delegator.TurnResult
	handler := &testHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completed = append(completed, r) },
	}
	applyHandler(b, handler)
	stateEvent(b, "running")

	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer,
		Text:   "actually, reconsider",
	}); err != nil {
		t.Fatalf("Inject(SourceSteer): %v", err)
	}
	if !strings.Contains(buf.String(), "reconsider") {
		t.Fatalf("steer not written to stdin: %q", buf.String())
	}

	// Abort result of the interrupted ask (5 output tokens generated so far).
	b.OnResult(&ResultMessage{Subtype: "success",
		Usage: TokenUsage{OutputTokens: 5}, ModelUsage: map[string]ModelUsage{}})
	if len(completed) != 0 || !b.IsTurnInFlight() {
		t.Fatal("abort result must neither complete nor release the turn")
	}

	// Second ask cycle: the steered reply.
	b.turnMu.Lock()
	b.turnText.WriteString("reconsidered answer")
	b.turnMu.Unlock()
	b.OnResult(&ResultMessage{Subtype: "success",
		Usage: TokenUsage{OutputTokens: 7}, ModelUsage: map[string]ModelUsage{}})

	stateEvent(b, "idle")

	if len(completed) != 1 {
		t.Fatalf("expected exactly one completion at idle; got %d", len(completed))
	}
	if completed[0].Text != "reconsidered answer" {
		t.Errorf("steered reply lost: %q", completed[0].Text)
	}
	if got := completed[0].Usage.OutputTokens; got != 12 {
		t.Errorf("output tokens must sum across ask cycles: got %d, want 12", got)
	}
}

// TestIdleKeyed_FoldedSteerSingleResult replays the fold shape (steer arrives
// mid-tool → CC folds it into the running ask, ONE result answers
// everything). Under the old counter/re-arm design this mode held the answer
// hostage for the 45s watchdog; now idle completes it immediately.
func TestIdleKeyed_FoldedSteerSingleResult(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}

	var completed []*delegator.TurnResult
	handler := &testHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completed = append(completed, r) },
	}
	applyHandler(b, handler)
	stateEvent(b, "running")

	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer, Text: "also cover the edge case",
	}); err != nil {
		t.Fatalf("Inject(SourceSteer): %v", err)
	}

	b.turnMu.Lock()
	b.turnText.WriteString("answer covering the edge case too")
	b.turnMu.Unlock()
	b.OnResult(&ResultMessage{Subtype: "success", ModelUsage: map[string]ModelUsage{}})
	stateEvent(b, "idle")

	if len(completed) != 1 {
		t.Fatalf("fold mode must complete at idle without any hold; got %d completions", len(completed))
	}
	if completed[0].Text != "answer covering the edge case too" {
		t.Errorf("folded answer lost: %q", completed[0].Text)
	}
}

// TestIdleKeyed_OrphanRun proves runs foci never opened a turn for (slash
// commands, task-notification runs after a background Bash finishes) pass
// through without panics and without firing anyone's OnTurnComplete.
func TestIdleKeyed_OrphanRun(t *testing.T) {
	t.Parallel()

	b := &Backend{}
	b.AttachSessionEvents(&delegator.SessionEvents{})

	stateEvent(b, "running")
	b.OnResult(&ResultMessage{Subtype: "success", Result: "orphan text", ModelUsage: map[string]ModelUsage{}})
	stateEvent(b, "idle")

	if b.IsTurnInFlight() {
		t.Error("orphan run must not leave a turn in flight")
	}
}

// TestIdleKeyed_FallbackWithoutStateEvents proves the legacy path: when CC
// has emitted no session_state_changed events this session (env unset, older
// binary, or the rare missed event before any was seen), the turn completes
// on the result — the pre-idle-keyed behaviour — so nothing hangs.
func TestIdleKeyed_FallbackWithoutStateEvents(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}

	var completed []*delegator.TurnResult
	handler := &testHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completed = append(completed, r) },
	}
	applyHandler(b, handler)
	// NO stateEvent — CC is not emitting them.

	b.turnMu.Lock()
	b.turnText.WriteString("fallback reply")
	b.turnMu.Unlock()
	b.OnResult(&ResultMessage{Subtype: "success", ModelUsage: map[string]ModelUsage{}})

	if len(completed) != 1 {
		t.Fatalf("without state events the result must complete the turn; got %d", len(completed))
	}
	if completed[0].Text != "fallback reply" {
		t.Errorf("wrong fallback result: %q", completed[0].Text)
	}
	if b.IsTurnInFlight() {
		t.Error("turn should be released by the fallback completion")
	}
}

// TestIdleKeyed_PreAnswerRedispatch proves the pre-answer verification nudge
// under the idle lifecycle: the gate runs at idle on the final stashed
// result; a returned follow-up is sent and HOLDS the turn open (including
// across a stray extra idle before CC picks the follow-up up); the
// follow-up's own result + idle complete the turn with the revised answer.
func TestIdleKeyed_PreAnswerRedispatch(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}

	fired := false
	var completed []*delegator.TurnResult
	handler := &testHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completed = append(completed, r) },
		PreAnswerNudgeFunc: func(_ *delegator.TurnResult) string {
			if fired {
				return "" // turn_delegated's closure fires once
			}
			fired = true
			return "double-check your answer"
		},
	}
	applyHandler(b, handler)
	stateEvent(b, "running")

	b.turnMu.Lock()
	b.turnText.WriteString("first answer")
	b.turnMu.Unlock()
	b.OnResult(&ResultMessage{Subtype: "success", ModelUsage: map[string]ModelUsage{}})
	stateEvent(b, "idle")

	if len(completed) != 0 {
		t.Fatal("idle with a pre-answer follow-up must not complete the turn")
	}
	if !strings.Contains(buf.String(), "double-check your answer") {
		t.Fatalf("follow-up not written to stdin: %q", buf.String())
	}
	if !b.IsTurnInFlight() {
		t.Fatal("turn must stay open across the re-dispatch")
	}

	// A stray idle before CC picks the follow-up up (the run ended before the
	// queued message was drained) must ALSO hold the turn open.
	stateEvent(b, "idle")
	if len(completed) != 0 || !b.IsTurnInFlight() {
		t.Fatal("stray idle during re-dispatch must not complete the turn")
	}

	// The follow-up's run: revised answer, then idle.
	stateEvent(b, "running")
	b.turnMu.Lock()
	b.turnText.WriteString("\n\nrevised answer")
	b.turnMu.Unlock()
	b.OnResult(&ResultMessage{Subtype: "success", ModelUsage: map[string]ModelUsage{}})
	stateEvent(b, "idle")

	if len(completed) != 1 {
		t.Fatalf("revised answer's idle should complete the turn; got %d completions", len(completed))
	}
	if !strings.Contains(completed[0].Text, "revised answer") {
		t.Errorf("revised answer lost: %q", completed[0].Text)
	}
}

// TestIdleKeyed_PreAnswerSendFailure proves a failed follow-up send degrades
// to completing with the first-round result instead of wedging the turn.
func TestIdleKeyed_PreAnswerSendFailure(t *testing.T) {
	t.Parallel()

	b := &Backend{writer: NewWriter(errWriteCloser{})}
	b.typingFunc = func(bool) {}

	var completed []*delegator.TurnResult
	handler := &testHandler{
		OnTurnComplete:     func(r *delegator.TurnResult) { completed = append(completed, r) },
		PreAnswerNudgeFunc: func(_ *delegator.TurnResult) string { return "verify" },
	}
	applyHandler(b, handler)
	stateEvent(b, "running")

	b.turnMu.Lock()
	b.turnText.WriteString("only answer")
	b.turnMu.Unlock()
	b.OnResult(&ResultMessage{Subtype: "success", ModelUsage: map[string]ModelUsage{}})
	stateEvent(b, "idle")

	if len(completed) != 1 {
		t.Fatalf("send failure must fall through to completion; got %d", len(completed))
	}
	if completed[0].Text != "only answer" {
		t.Errorf("first-round result lost on send failure: %q", completed[0].Text)
	}
}

// TestIdleKeyed_WaitForTurnUnblocksAtIdle proves WaitForTurn tracks the new
// boundary: it stays blocked across a result and returns once idle completes
// the turn.
func TestIdleKeyed_WaitForTurnUnblocksAtIdle(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}
	applyHandler(b, &testHandler{OnTurnComplete: func(*delegator.TurnResult) {}})
	stateEvent(b, "running")

	b.OnResult(&ResultMessage{Subtype: "success", ModelUsage: map[string]ModelUsage{}})

	// Still blocked after the result — the turn is not complete yet.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := b.WaitForTurn(ctx); err == nil {
		t.Fatal("WaitForTurn should still block after a result (turn completes at idle)")
	}

	stateEvent(b, "idle")
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := b.WaitForTurn(ctx2); err != nil {
		t.Fatalf("WaitForTurn should return once idle completed the turn: %v", err)
	}
}
