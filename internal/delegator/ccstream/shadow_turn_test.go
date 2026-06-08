package ccstream

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
)

// ---------------------------------------------------------------------------
// Steer "shadow turn" delivery-loss regression (#813)
// ---------------------------------------------------------------------------
//
// Scenario (the bug, from clutch/docs/steer-shadow-turn-delivery-loss.md):
//
// A user message arriving mid-turn is injected as a SourceSteer. CC does NOT
// silently fold it into the running ask(); instead the stdin write makes CC
// emit an immediate result (frequently output=0 — the first turn produced
// nothing yet), and CC then reprocesses the steered context as a *fresh*
// response. foci sees that first result in OnResult and — today — COMPLETES
// the turn: it clears turnActive, releases the agent in-flight refcount (via
// OnTurnComplete), and detaches the delivering sink.
//
// CC's real steered reply then arrives as a SECOND OnResult with no live turn
// to land in — a "shadow turn". During the window between the two results foci
// believes the session is idle, so a colliding non-user inject (a reflection /
// keepalive pass) can begin a fresh turn on top of the in-progress shadow
// reply and route it to a non-delivering sink. The user's reply is generated
// but never delivered.
//
// The fix (Phase 3): a folded steer/user inject must re-arm the turn (the same
// reArmForContinuation path the pre-answer nudge already uses) so the turn
// stays in flight until CC's steered reply completes — keeping the in-flight
// refcount held and the delivering sink attached across the shadow window.
//
// This test drives the real Inject(SourceSteer) path and asserts that
// invariant. It is RED against current code (the first OnResult completes the
// turn prematurely and the real reply never reaches OnTurnComplete) and turns
// GREEN once the folded-steer re-arm lands.
func TestSteerFold_KeepsTurnInFlight_UntilShadowReply(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer: NewWriter(nopWriteCloser{&buf}),
	}
	b.typingFunc = func(bool) {}

	var completedCount int
	var completedResult *delegator.TurnResult
	handler := &delegator.EventHandler{
		OnTurnComplete: func(r *delegator.TurnResult) {
			completedCount++
			completedResult = r
		},
	}
	applyHandler(b, handler) // begins the turn; turn is now in flight

	if !b.IsTurnInFlight() {
		t.Fatal("precondition: turn should be in flight after begin")
	}

	// The first turn has produced no text yet (the real incident's first
	// result was output=0). A mid-turn steer arrives.
	if err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer,
		Text:   "actually, reconsider the causal theory",
	}); err != nil {
		t.Fatalf("Inject(SourceSteer) failed: %v", err)
	}
	if !strings.Contains(buf.String(), "reconsider the causal theory") {
		t.Errorf("steer should have been written to CC stdin (folded), got: %q", buf.String())
	}

	// Round 1: the immediate result the steer write triggers (output=0 — the
	// first turn generated nothing). This MUST NOT complete the turn: CC's
	// actual steered reply is still coming as a second result, and it needs a
	// live, delivering turn to land in.
	b.OnResult(&ResultMessage{Subtype: "success", Result: "", ModelUsage: map[string]ModelUsage{}})

	if completedCount != 0 {
		t.Errorf("OnTurnComplete must NOT fire on the steer-triggered result "+
			"(shadow window opens here); fired %d times", completedCount)
	}
	if !b.IsTurnInFlight() {
		t.Error("turn must stay in flight across the shadow window so the " +
			"steered reply has a delivering sink (this is the #813 fix)")
	}

	// Round 2: CC's real steered reply completes. THIS is the result that
	// should finalise the turn and deliver the user's answer.
	b.turnMu.Lock()
	b.turnText.WriteString("here is the reply Dick must actually receive")
	b.turnMu.Unlock()
	b.OnResult(&ResultMessage{Subtype: "success", Result: "", ModelUsage: map[string]ModelUsage{}})

	if completedCount != 1 {
		t.Errorf("OnTurnComplete should fire exactly once, on the steered reply; got %d", completedCount)
	}
	if completedResult == nil || completedResult.Text != "here is the reply Dick must actually receive" {
		t.Errorf("steered reply lost: completedResult = %+v", completedResult)
	}
	if b.IsTurnInFlight() {
		t.Error("turn should be complete after the steered reply lands")
	}
}

// TestSteerFold_WatchdogReleasesWhenNoShadowReply is the safety net for the
// case the happy path can't cover: a folded steer produces NO second OnResult
// at all (CC folds it into the same ask() with a single result). Re-arming
// would otherwise leave the turn in flight until the orchestrator's 24h
// streamIdleTimeout. The watchdog must force-complete within its bound and
// deliver the stashed round-1 result — which, in this fold mode, IS the answer.
func TestSteerFold_WatchdogReleasesWhenNoShadowReply(t *testing.T) {
	// Not parallel: drives a real timer; avoid starvation under -parallel load.
	var buf bytes.Buffer
	b := &Backend{
		writer:             NewWriter(nopWriteCloser{&buf}),
		reArmWatchdogBound: 40 * time.Millisecond,
	}
	b.typingFunc = func(bool) {}

	var completedCount int32
	done := make(chan *delegator.TurnResult, 1)
	handler := &delegator.EventHandler{
		OnTurnComplete: func(r *delegator.TurnResult) {
			atomic.AddInt32(&completedCount, 1)
			done <- r
		},
	}
	applyHandler(b, handler)

	// Round 1 carries the real answer (the single-OnResult fold mode).
	b.turnMu.Lock()
	b.turnText.WriteString("the answer that must not be lost")
	b.turnMu.Unlock()

	if err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer,
		Text:   "tweak it",
	}); err != nil {
		t.Fatalf("Inject(SourceSteer): %v", err)
	}
	// The one and only result the steer triggers — re-arms and arms the watchdog.
	b.OnResult(&ResultMessage{Subtype: "success", Result: "", ModelUsage: map[string]ModelUsage{}})

	// No second OnResult ever arrives. The watchdog must release the turn.
	select {
	case r := <-done:
		if r == nil || r.Text != "the answer that must not be lost" {
			t.Errorf("watchdog must deliver the stashed round-1 result; got %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog never fired — turn would hang until streamIdleTimeout (#813)")
	}
	if b.IsTurnInFlight() {
		t.Error("turn must be released after the watchdog fires")
	}
	// Give any stray second fire a chance to land, then assert exactly-once.
	time.Sleep(80 * time.Millisecond)
	if got := atomic.LoadInt32(&completedCount); got != 1 {
		t.Errorf("OnTurnComplete must fire exactly once, got %d", got)
	}
}

// TestSteerFold_WatchdogDefersWhileOutstandingPrompt proves the watchdog does
// not force-complete a re-armed turn that is blocked on a human (tool
// permission, AskUserQuestion, MCP elicitation). Such a wait emits NO stream
// events in pipe mode, so LastActivity goes stale and the activity-aware check
// alone would wrongly fire — truncating a turn that is alive and waiting. The
// real helen incident (2026-06-08): a watchdog fired exactly bound-seconds
// after an AskUserQuestion prompt. Once the prompt resolves and silence
// persists, the watchdog must still release the turn (#813).
func TestSteerFold_WatchdogDefersWhileOutstandingPrompt(t *testing.T) {
	// Not parallel: real-timer test.
	var buf bytes.Buffer
	b := &Backend{
		writer:             NewWriter(nopWriteCloser{&buf}),
		reArmWatchdogBound: 60 * time.Millisecond,
		outstanding:        NewOutstandingRegistry(),
	}
	b.typingFunc = func(bool) {}

	var completedCount int32
	done := make(chan struct{}, 1)
	handler := &delegator.EventHandler{
		OnTurnComplete: func(_ *delegator.TurnResult) {
			atomic.AddInt32(&completedCount, 1)
			select {
			case done <- struct{}{}:
			default:
			}
		},
	}
	applyHandler(b, handler)

	if err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer,
		Text:   "steer",
	}); err != nil {
		t.Fatalf("Inject(SourceSteer): %v", err)
	}
	// Re-arm + arm the watchdog, then immediately register an outstanding prompt
	// (the steered reply called AskUserQuestion and is now blocked on the human).
	b.OnResult(&ResultMessage{Subtype: "success", Result: "", ModelUsage: map[string]ModelUsage{}})
	b.outstanding.Register("req-q", OutstandingPermission)

	// Well past 2× the bound with the prompt still outstanding: the turn must NOT
	// be force-completed, even though no activity is touched (the human is thinking).
	time.Sleep(150 * time.Millisecond)
	if atomic.LoadInt32(&completedCount) != 0 {
		t.Fatal("watchdog fired while a prompt was outstanding — would truncate a turn blocked on the human")
	}
	if !b.IsTurnInFlight() {
		t.Fatal("turn must stay in flight while blocked on an outstanding prompt")
	}

	// Human answers → prompt resolves. With no further activity, the next tick
	// sees a stale LastActivity and must release the turn within ~one bound.
	b.outstanding.Resolve("req-q")
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog never fired after the outstanding prompt resolved")
	}
	if b.IsTurnInFlight() {
		t.Error("turn must be released once the prompt clears and silence persists")
	}
}

// TestSteerFold_WatchdogDefersWhileActive proves the watchdog is activity-aware:
// a re-armed turn that is legitimately busy (CC streaming / awaiting a tool
// permission — which emits nothing in pipe mode but does touch activity on its
// surrounding events) must NOT be force-completed while activity is fresh. Only
// sustained silence releases it.
func TestSteerFold_WatchdogDefersWhileActive(t *testing.T) {
	// Not parallel: real-timer test.
	var buf bytes.Buffer
	b := &Backend{
		writer:             NewWriter(nopWriteCloser{&buf}),
		reArmWatchdogBound: 60 * time.Millisecond,
	}
	b.typingFunc = func(bool) {}

	var completedCount int32
	done := make(chan struct{}, 1)
	handler := &delegator.EventHandler{
		OnTurnComplete: func(_ *delegator.TurnResult) {
			atomic.AddInt32(&completedCount, 1)
			select {
			case done <- struct{}{}:
			default:
			}
		},
	}
	applyHandler(b, handler)

	if err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer,
		Text:   "steer",
	}); err != nil {
		t.Fatalf("Inject(SourceSteer): %v", err)
	}
	b.OnResult(&ResultMessage{Subtype: "success", Result: "", ModelUsage: map[string]ModelUsage{}})

	// Keep CC "active" past the bound: touch activity every 25ms for ~150ms.
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				b.touchActivity()
			case <-stop:
				return
			}
		}
	}()

	// Well past the 60ms bound, the turn must still be alive (activity is fresh).
	time.Sleep(150 * time.Millisecond)
	if atomic.LoadInt32(&completedCount) != 0 {
		close(stop)
		t.Fatal("watchdog fired while CC was active — would truncate a busy re-armed turn")
	}
	if !b.IsTurnInFlight() {
		close(stop)
		t.Fatal("turn must stay in flight while active")
	}

	// Activity stops → the watchdog must now release within ~one bound.
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog never fired after activity ceased")
	}
}
