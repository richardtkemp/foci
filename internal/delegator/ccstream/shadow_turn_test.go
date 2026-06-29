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
	handler := &testHandler{
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
	handler := &testHandler{
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
		outstanding:        delegator.NewOutstandingRegistry(),
	}
	b.typingFunc = func(bool) {}

	var completedCount int32
	done := make(chan struct{}, 1)
	handler := &testHandler{
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
	b.outstanding.Register("req-q", delegator.OutstandingPermission)

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
	handler := &testHandler{
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

// initRaw builds a minimal system/init stream message — CC's per-continuation
// re-init herald. session_id is fixed; the design keys off the EVENT, not the id.
func initRaw() []byte {
	return []byte(`{
		"type": "system",
		"subtype": "init",
		"claude_code_version": "1.0.27",
		"cwd": "/tmp",
		"model": "claude-sonnet-4-20250514",
		"permissionMode": "default",
		"tools": ["Bash"],
		"session_id": "sess-shadow-001"
	}`)
}

// TestSteerFold_BurstFoldsToSingleReArm_NoWatchdog is the headline Phase-3
// regression for TODO #933. The OLD trigger was a per-steer COUNTER: a BURST of
// N steers landing in one inter-result gap incremented pendingSteer N times, but
// CC folds them into ONE re-init / ONE shadow result — so after the abort result
// and the single reply the counter was still > 0, re-arming to await phantom
// results that never come → the 45s watchdog fired on a clean turn.
//
// The fix drives re-arm off CC's own `system init` stream (init count == result
// count, IRIR) plus a set-ONCE foldPending gate. A burst now folds to a single
// re-arm: abort result → re-arm; mid-turn init → continuation expected; reply →
// COMPLETE. Exactly one completion, the turn is released synchronously on the
// reply, and the watchdog is never needed.
func TestSteerFold_BurstFoldsToSingleReArm_NoWatchdog(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer: NewWriter(nopWriteCloser{&buf}),
		// Long bound: if the burst regressed into an over-re-arm, the turn would
		// hang here (not complete synchronously below) rather than racing a timer.
		reArmWatchdogBound: 30 * time.Second,
		readyCh:            make(chan struct{}),
	}
	b.typingFunc = func(bool) {}

	var completedCount int
	var completedResult *delegator.TurnResult
	handler := &testHandler{
		OnTurnComplete: func(r *delegator.TurnResult) {
			completedCount++
			completedResult = r
		},
	}
	applyHandler(b, handler)

	// A BURST: three steers land before any result separates them (the real
	// "inbox: urgent dispatch" stacking that produced the live 24× watchdog
	// trace). markFoldedInject sets foldPending once, regardless of count.
	for i, txt := range []string{"reconsider A", "and B", "and also C"} {
		if err := b.Inject(context.Background(), delegator.Inject{
			Source: delegator.SourceSteer,
			Text:   txt,
		}); err != nil {
			t.Fatalf("Inject(SourceSteer) #%d failed: %v", i+1, err)
		}
	}

	// Abort result (output=0): CC drains the burst, emits the current ask()'s
	// result, then re-inits for the single continuation. MUST re-arm, not complete.
	b.OnResult(&ResultMessage{Subtype: "success", Result: "", ModelUsage: map[string]ModelUsage{}})
	if completedCount != 0 {
		t.Fatalf("abort result must not complete the turn; completed %d times", completedCount)
	}
	if !b.IsTurnInFlight() {
		t.Fatal("turn must stay in flight across the shadow window")
	}

	// CC re-initialises for the (single) continuation cycle.
	b.OnSystem("init", initRaw())
	b.turnMu.Lock()
	if !b.continuationExpected {
		t.Error("mid-turn init must mark continuationExpected")
	}
	b.turnMu.Unlock()

	// The one folded reply (covers all three steers). This MUST complete the turn
	// synchronously — the old counter would re-arm again and wait for phantom
	// results (R#3/R#4), leaving completedCount==0 and the turn in flight.
	b.turnMu.Lock()
	b.turnText.WriteString("single reply covering A, B and C")
	b.turnMu.Unlock()
	b.OnResult(&ResultMessage{Subtype: "success", Result: "", ModelUsage: map[string]ModelUsage{}})

	if completedCount != 1 {
		t.Errorf("burst must complete exactly once on the reply (no phantom-result re-arm); got %d", completedCount)
	}
	if completedResult == nil || completedResult.Text != "single reply covering A, B and C" {
		t.Errorf("folded reply lost: %+v", completedResult)
	}
	if b.IsTurnInFlight() {
		t.Error("turn must be released synchronously on the reply — the watchdog was not needed")
	}
}

// TestSteerFold_ChainedFold_ReArmsAgain proves a steer arriving DURING the shadow
// window (after the first re-arm, before the reply) re-arms a second time and the
// turn still completes exactly once. The continuation result consumes the
// mid-turn-init herald; because another fold is pending (the in-window steer), the
// turn stays open for that steer's own abort+reinit cycle, then completes on the
// final reply. The watchdog backstops any cycle that fails to materialise.
func TestSteerFold_ChainedFold_ReArmsAgain(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:             NewWriter(nopWriteCloser{&buf}),
		reArmWatchdogBound: 30 * time.Second,
		readyCh:            make(chan struct{}),
	}
	b.typingFunc = func(bool) {}

	var completedCount int
	handler := &testHandler{
		OnTurnComplete: func(_ *delegator.TurnResult) { completedCount++ },
	}
	applyHandler(b, handler)

	// Steer A → abort A → re-arm.
	if err := b.Inject(context.Background(), delegator.Inject{Source: delegator.SourceSteer, Text: "steer A"}); err != nil {
		t.Fatalf("Inject A: %v", err)
	}
	b.OnResult(&ResultMessage{Subtype: "success", Result: "", ModelUsage: map[string]ModelUsage{}})
	if completedCount != 0 || !b.IsTurnInFlight() {
		t.Fatalf("after abort A: completed=%d inFlight=%v, want 0/true", completedCount, b.IsTurnInFlight())
	}

	// CC re-inits for A's continuation; a NEW steer B lands during the window.
	b.OnSystem("init", initRaw())
	if err := b.Inject(context.Background(), delegator.Inject{Source: delegator.SourceSteer, Text: "steer B"}); err != nil {
		t.Fatalf("Inject B (in shadow window): %v", err)
	}

	// Abort B result: consumes A's init herald, but B is pending → re-arm AGAIN.
	b.OnResult(&ResultMessage{Subtype: "success", Result: "", ModelUsage: map[string]ModelUsage{}})
	if completedCount != 0 {
		t.Fatalf("chained fold must re-arm again, not complete; completed %d", completedCount)
	}
	if !b.IsTurnInFlight() {
		t.Fatal("turn must stay in flight for B's continuation")
	}

	// CC re-inits for B's continuation; the final reply lands → COMPLETE once.
	b.OnSystem("init", initRaw())
	b.turnMu.Lock()
	b.turnText.WriteString("final reply after the chained fold")
	b.turnMu.Unlock()
	b.OnResult(&ResultMessage{Subtype: "success", Result: "", ModelUsage: map[string]ModelUsage{}})

	if completedCount != 1 {
		t.Errorf("chained fold must complete exactly once; got %d", completedCount)
	}
	if b.IsTurnInFlight() {
		t.Error("turn should be released after the final reply")
	}
}
