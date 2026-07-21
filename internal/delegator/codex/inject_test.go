package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
)

func newInjectTestBackend(failWrites bool) (*Backend, *captureCloser) {
	c := &captureCloser{fail: failWrites}
	b := &Backend{
		lg:         log.NewComponentLogger("codex"),
		threadID:   "thread-test",
		workDir:    "/tmp",
		pendingRPC: make(map[int64]chan rpcReply),
	}
	b.writer = NewWriter(c)
	return b, c
}

// openTurn arms the turn-state a real beginTurn would, minus the turn/start
// round-trip: turnActive is set, TurnEvents installed, and a fresh
// turnResultCh allocated. Used by tests that exercise completion / WaitForTurn
// / in-flight routing in isolation, without the beginTurn sender goroutine.
func openTurn(b *Backend, turn *delegator.TurnEvents) {
	b.turnMu.Lock()
	b.turnActive = true
	b.turnEvents = turn
	b.turnResultCh = make(chan *delegator.TurnResult, 1)
	b.turnText.Reset()
	b.turnTools = 0
	b.turnMu.Unlock()
}

// TestBeginTurn_SetsStateResetsScratch proves beginTurn's synchronous setup:
// it flips turnActive, allocates a fresh turnResultCh, installs TurnEvents,
// and — critically — wipes the scratch fields a previous turn left behind so
// stale text/usage can't leak into the new turn.
func TestBeginTurn_NoThreadErrors(t *testing.T) {
	t.Parallel()

	b, _ := newInjectTestBackend(false)
	b.mu.Lock()
	b.threadID = ""
	b.mu.Unlock()

	err := b.beginTurn("hello", &delegator.TurnEvents{OnTurnComplete: func(*delegator.TurnResult) {}})
	if err == nil || !strings.Contains(err.Error(), "no active thread") {
		t.Fatalf("beginTurn with no thread = %v, want no-active-thread error", err)
	}
}

func TestBeginTurn_SendFailureCompletesTurn(t *testing.T) {
	t.Parallel()

	b, _ := newInjectTestBackend(true)

	var completed *delegator.TurnResult
	err := b.beginTurn("hello", &delegator.TurnEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { completed = r },
	})
	if err == nil {
		t.Fatal("beginTurn with failing writer should error")
	}
	if completed == nil {
		t.Fatal("OnTurnComplete should fire on send failure")
	}
	if b.IsTurnInFlight() {
		t.Error("turn should not be in flight after failed beginTurn")
	}
}

// TestBeginTurn_CommitsPendingModel proves a successful turn/start makes the
// resolved per-session override the backend's reported model, preventing the
// next TurnResult from reverting foci metadata to the previous model.
func TestBeginTurn_CommitsPendingModel(t *testing.T) {
	b := setupMockBackend(t, func(method string, params json.RawMessage, _ int64) (json.RawMessage, error) {
		if method != "turn/start" {
			t.Fatalf("method = %q, want turn/start", method)
		}
		return json.RawMessage(`{"turn":{"id":"turn-model","status":"inProgress"}}`), nil
	})
	b.threadID = "thread-model"
	b.pendingModel = "gpt-5.6-luna"
	if err := b.beginTurn("hello", &delegator.TurnEvents{}); err != nil {
		t.Fatalf("beginTurn: %v", err)
	}
	b.mu.Lock()
	got := b.model
	b.mu.Unlock()
	if got != "gpt-5.6-luna" {
		t.Errorf("backend model = %q", got)
	}
}

// TestIsTurnInFlight_Lifecycle pins the flag callers gate injects on: false on
// a fresh backend, true once beginTurn opens a turn, false again once
// completeTurn releases it.
func TestIsTurnInFlight_Lifecycle(t *testing.T) {
	t.Parallel()

	b, _ := newInjectTestBackend(false)

	if b.IsTurnInFlight() {
		t.Fatal("fresh backend must report no turn in flight")
	}

	openTurn(b, &delegator.TurnEvents{})
	if !b.IsTurnInFlight() {
		t.Fatal("IsTurnInFlight must be true after openTurn")
	}

	b.completeTurn(&delegator.TurnResult{Text: "done"})
	if b.IsTurnInFlight() {
		t.Fatal("IsTurnInFlight must be false after completeTurn")
	}
}

// TestCompleteTurn_FiresOnTurnCompleteWithAccumulatedTextAndUsage proves the
// completion path delivers what the reader accumulated — completed
// agentMessage items summed into turnText and the latest tokenUsage stashed
// — through to OnTurnComplete, then clears the turn bookkeeping.
//
// Text deltas (onAgentMessageDelta) are replayed too, matching a real turn's
// event order (deltas stream, then the item completes with its full text),
// but deliberately NOT summed into the expected text: deltas are live-display
// only (see onAgentMessageDelta's doc) — accumulating both there and here
// double-counted every message (#1329 item 6 finding).
func TestCompleteTurn_FiresOnTurnCompleteWithAccumulatedTextAndUsage(t *testing.T) {
	t.Parallel()

	b, _ := newInjectTestBackend(false)

	var got *delegator.TurnResult
	openTurn(b, &delegator.TurnEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { got = r },
	})

	// Replay a real turn's event order: deltas stream live, then the item
	// completes with its full final text.
	b.onAgentMessageDelta(&agentMessageDeltaParams{Delta: "Hello, "})
	b.onAgentMessageDelta(&agentMessageDeltaParams{Delta: "world."})
	b.onItemCompleted(&itemCompletedParams{Item: mustItem(t, itemEnvelope{
		Type: "agentMessage", ID: "m1", Text: "Hello, world.",
	})})

	tup := &tokenUsageParams{}
	tup.TokenUsage.Last.InputTokens = 10
	tup.TokenUsage.Last.OutputTokens = 4
	tup.TokenUsage.Last.CachedInputTokens = 2
	b.onTokenUsage(tup)

	// Build the result from the accumulated scratch exactly as onTurnCompleted
	// does, then hand it to completeTurn.
	b.turnMu.Lock()
	usage := b.stashedUsage
	text := b.turnText.String()
	b.turnMu.Unlock()
	b.completeTurn(&delegator.TurnResult{Text: text, Usage: usage})

	if got == nil {
		t.Fatal("OnTurnComplete was not fired")
	}
	if got.Text != "Hello, world." {
		t.Errorf("Text = %q, want %q", got.Text, "Hello, world.")
	}
	if got.Usage == nil {
		t.Fatal("Usage not delivered")
	}
	if got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 4 || got.Usage.CacheReadInputTokens != 2 {
		t.Errorf("Usage = %+v, want in=10 out=4 cache-read=2", got.Usage)
	}

	// completeTurn must also release the turn.
	b.turnMu.Lock()
	active := b.turnActive
	ch := b.turnResultCh
	b.turnMu.Unlock()
	if active {
		t.Error("completeTurn must clear turnActive")
	}
	if ch != nil {
		t.Error("completeTurn must nil turnResultCh")
	}
}

// TestWaitForTurn_BlocksUntilCompletion proves WaitForTurn tracks the turn
// boundary: with a turn open it stays parked, and it returns nil once
// completeTurn signals the result channel.
func TestWaitForTurn_BlocksUntilCompletion(t *testing.T) {
	t.Parallel()

	b, _ := newInjectTestBackend(false)

	// With no turn open WaitForTurn returns immediately (no channel to wait on).
	quickCtx, quickCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer quickCancel()
	if err := b.WaitForTurn(quickCtx); err != nil {
		t.Fatalf("WaitForTurn with no turn in flight = %v, want nil", err)
	}

	openTurn(b, &delegator.TurnEvents{})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.WaitForTurn(ctx) }()

	// Must still be blocked before completion fires.
	select {
	case err := <-done:
		t.Fatalf("WaitForTurn returned before completion: %v", err)
	case <-time.After(50 * time.Millisecond):
		// good — parked on the result channel.
	}

	b.completeTurn(&delegator.TurnResult{Text: "finished"})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForTurn after completion = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitForTurn did not unblock after completeTurn")
	}
}

// TestImmediateInject_SourceCompact_TriggersCompaction proves the /compact
// slash-command source routes to triggerCompaction (thread/compact/start),
// never to beginTurn — observed via the captured request and the turn staying
// unopened.
func TestImmediateInject_SourceCompact_TriggersCompaction(t *testing.T) {
	t.Parallel()

	// failWrites=true: sendAndWait returns at once after recording the request,
	// so triggerCompaction completes within the test instead of blocking 30s.
	b, c := newInjectTestBackend(true)

	err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceCompact, Text: "/compact",
	})
	if err == nil {
		t.Fatal("SourceCompact should surface the (test-injected) send error")
	}
	if !strings.Contains(c.String(), "thread/compact/start") {
		t.Errorf("SourceCompact must dispatch thread/compact/start; buf=%q", c.String())
	}
	if b.IsTurnInFlight() {
		t.Error("SourceCompact must not open a turn")
	}
}

// TestImmediateInject_SourceCompact_NoThreadErrors proves the same source
// hits triggerCompaction's no-thread guard — the distinctive error only that
// function returns — confirming the routing even without a live app-server.
func TestImmediateInject_SourceCompact_NoThreadErrors(t *testing.T) {
	t.Parallel()

	b, _ := newInjectTestBackend(true)
	b.mu.Lock()
	b.threadID = "" // no active thread → triggerCompaction's guard fires
	b.mu.Unlock()

	err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceCompact, Text: "/compact",
	})
	if err == nil || !strings.Contains(err.Error(), "no active thread") {
		t.Fatalf("SourceCompact with no thread must hit triggerCompaction's guard; got %v", err)
	}
}

// TestImmediateInject_SourcePassIgnored proves passthrough slash commands are
// dropped on the floor: no write to the app-server and no change to turn state.
func TestImmediateInject_SourcePassIgnored(t *testing.T) {
	t.Parallel()

	b, c := newInjectTestBackend(true)
	openTurn(b, &delegator.TurnEvents{}) // even with a turn open…

	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourcePass, Text: "/context",
	}); err != nil {
		t.Fatalf("SourcePass = %v, want nil", err)
	}
	if c.String() != "" {
		t.Errorf("SourcePass must not write anything; buf=%q", c.String())
	}
	if !b.IsTurnInFlight() {
		t.Error("SourcePass must not alter turn state")
	}
}

// TestImmediateInject_InFlight_SourceSystemRejected proves system-initiated
// input (foci send, cron, notifications) never folds into a running turn:
// it is rejected with ErrTurnInFlight so the caller waits and retries.
func TestImmediateInject_InFlight_SourceSystemRejected(t *testing.T) {
	t.Parallel()

	b, c := newInjectTestBackend(true)
	openTurn(b, &delegator.TurnEvents{})

	err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSystem, Text: "keepalive",
	})
	if !errors.Is(err, delegator.ErrTurnInFlight) {
		t.Fatalf("SourceSystem while in flight = %v, want ErrTurnInFlight", err)
	}
	if c.String() != "" {
		t.Errorf("rejected SourceSystem must not write anything; buf=%q", c.String())
	}
	if !b.IsTurnInFlight() {
		t.Error("rejected inject must leave the turn in flight")
	}
}

// TestImmediateInject_InFlight_SteerAndUserFold proves the fold sources —
// SourceSteer (platform dispatch) and SourceUser (queued user text) — route to
// steerTurn when a turn is running, dispatching turn/steer with the injected
// text AND the required expectedTurnId precondition (item 1 — omitting this
// field, the prior implementation, was rejected outright by a live
// app-server), leaving the turn open once codex accepts it.
func TestImmediateInject_InFlight_SteerAndUserFold(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		source delegator.InjectSource
	}{
		{"SourceSteer", delegator.SourceSteer},
		{"SourceUser", delegator.SourceUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod string
			var gotParams turnSteerParams
			b := setupMockBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
				gotMethod = method
				_ = json.Unmarshal(params, &gotParams)
				return json.RawMessage(`{"turnId":"turn-active"}`), nil
			})
			openTurn(b, &delegator.TurnEvents{})
			b.turnMu.Lock()
			b.turnID = "turn-active"
			b.turnMu.Unlock()

			if err := b.ImmediateInject(context.Background(), delegator.Inject{
				Source: tc.source,
				Text:   "reconsider " + tc.name,
			}); err != nil {
				t.Fatalf("ImmediateInject = %v, want nil", err)
			}

			if gotMethod != "turn/steer" {
				t.Errorf("method = %q, want turn/steer", gotMethod)
			}
			if gotParams.ExpectedTurnID != "turn-active" {
				t.Errorf("expectedTurnId = %q, want %q", gotParams.ExpectedTurnID, "turn-active")
			}
			if len(gotParams.Input) != 1 || gotParams.Input[0].Text != "reconsider "+tc.name {
				t.Errorf("input = %+v, want [{text %q}]", gotParams.Input, "reconsider "+tc.name)
			}
			if !b.IsTurnInFlight() {
				t.Error("steer must keep the turn in flight")
			}
		})
	}
}

// TestSteerTurn_RaceAfterTurnCompleted is the red/green regression for #1329
// item 1: verified live against codex app-server 0.144.5 that a turn/steer
// landing after the turn it targeted already completed is rejected with
// "no active turn to steer" (expectedTurnId no longer matches). Before the
// fix, steerTurn ran fire-and-forget in a goroutine and ImmediateInject
// returned nil immediately regardless — the rejection was only Warnf'd, so
// the caller (and the user) never learned the mid-turn message was lost.
// Now steerTurn runs synchronously and this specific rejection surfaces as
// delegator.ErrTurnNotInFlight — the same sentinel ccstream's Steer-at-idle
// race uses — which inbox.go's existing re-route already turns into a fresh,
// properly-tracked turn instead of silently dropping the text.
func TestSteerTurn_RaceAfterTurnCompleted(t *testing.T) {
	t.Parallel()

	b := setupMockBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
		if method != "turn/steer" {
			t.Fatalf("method = %q, want turn/steer", method)
		}
		return nil, rpcErrorReply{code: -32600, message: "no active turn to steer"}
	})
	openTurn(b, &delegator.TurnEvents{})
	b.turnMu.Lock()
	b.turnID = "turn-that-just-ended"
	b.turnMu.Unlock()

	err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer,
		Text:   "mid-turn message",
	})
	if !errors.Is(err, delegator.ErrTurnNotInFlight) {
		t.Fatalf("ImmediateInject(Steer) after turn completion = %v, want ErrTurnNotInFlight", err)
	}
}

// TestSteerTurn_OtherErrorsSurfaced proves a turn/steer failure that ISN'T
// the turn-already-ended race (a protocol error, dead connection, etc.) is
// returned to the caller rather than swallowed — inbox.go's default case
// already queues the text for a fresh turn on any non-nil error, so nothing
// is lost, but only if the error actually gets there instead of being
// discarded inside a fire-and-forget goroutine as before.
func TestSteerTurn_OtherErrorsSurfaced(t *testing.T) {
	t.Parallel()

	b := setupMockBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
		return nil, rpcErrorReply{code: -32000, message: "internal error"}
	})
	openTurn(b, &delegator.TurnEvents{})
	b.turnMu.Lock()
	b.turnID = "turn-1"
	b.turnMu.Unlock()

	err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer,
		Text:   "steer text",
	})
	if err == nil {
		t.Fatal("a genuine turn/steer failure must be surfaced, got nil")
	}
	if errors.Is(err, delegator.ErrTurnNotInFlight) {
		t.Error("a non-race error must not be reported as ErrTurnNotInFlight")
	}
}
