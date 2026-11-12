package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"foci/internal/delegator"
	"foci/internal/platform"
	"foci/internal/turn"
	"foci/internal/turnevent"
)

// runInjectedTurnWithLateText drives a real delegated injected turn whose ctx
// sink is a turn.SessionSink (as agents_notify / session_notify / wakes build
// it) and has the backend emit text AFTER the turn completed but BEFORE the
// turn's router registration is released (#2994). That window is post-turn
// (runPostTurn): the turn's CompletionChan is closed, but the orchestrator's
// Phase 3.5 release runs only when post-turn returns. Text a backend's reader
// emits in that window and that no newer registration claims reaches this
// turn's sink. If the turn's final text was never streamed, that text sets the
// sink's delivered flag and the turn's FinalText is suppressed: a lost reply.
// In production that cannot happen on ccstream, because CC's next run is
// adopted (its own sink registered) before its first text; see
// TestSessionSink_NextRunAdoptedBeforeItsText_UnstreamedFinalKept. With
// adoptFirst=false the test models a backend with no running edge (ccstream
// without session-state events, as cc-stub runs it), and loses the reply.
//
// The window is made deterministic with the post-turn activity hook
// (TouchActivityPost → a.OnActivity), which runs on the session worker inside
// it: the hook emits the late text synchronously, so the text is routed while
// this turn's registration is provably still current.
//
// streamed is the text the turn's own run streamed (OnText) before it ended; ""
// means the turn's final text was never streamed (CC result text with no
// assistant text block, a process-exit notice). final is the TurnResult text.
// adoptFirst opens the next run as an adopted autonomous turn before its text,
// as ccstream's running edge does.
func runInjectedTurnWithLateText(t *testing.T, streamed, final, late string, adoptFirst bool) []string {
	t.Helper()
	initTestConvo(t, "helen")
	const sk = "helen/c42"
	be := &adoptingBackend{}
	be.sessionFile = "/tmp/late-text-2994.jsonl"

	var a *Agent
	var completed, lateSent atomic.Bool
	be.sendToPaneFn = func(_ context.Context, _ string, h *mockHandler) (*delegator.TurnResult, error) {
		se := be.events()
		go func() {
			if !waitRegistered(t, a, sk) {
				h.OnTurnComplete(&delegator.TurnResult{Text: final, Model: "claude-sonnet-4-5"})
				return
			}
			if streamed != "" {
				se.OnText(streamed)
			}
			completed.Store(true)
			h.OnTurnComplete(&delegator.TurnResult{Text: final, Model: "claude-sonnet-4-5"})
		}()
		return nil, nil
	}
	a = newBatchTestAgent(t, be)
	lateConn := &leakConn{}
	a.ResolveLateConn = func(string) platform.Connection { return lateConn }
	a.OnActivity.Add(func(string) {
		if !completed.Load() || !lateSent.CompareAndSwap(false, true) {
			return
		}
		if a.sessionRouter(sk).current.Load() == nil {
			t.Error("precondition: the turn's registration was already released when the late text arrived")
		}
		if adoptFirst {
			// CC's running edge for its next run: ccstream fires
			// onAutonomousOpen synchronously on the reader goroutine, before
			// the run's first text (handlers.go session_state_changed).
			a.OpenAutonomousTurn(sk, be, nil)
		}
		be.events().OnText(late)
	})

	conn := &leakConn{}
	sink := turn.NewSessionSink(conn, sk, "async_notify")
	ctx := turnevent.WithSink(context.Background(), sink)
	if err := a.HandleMessage(ctx, sk, []string{"[foci send] check the build"}, nil); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if !lateSent.Load() {
		t.Fatal("precondition: the late text was never emitted inside the post-turn window")
	}
	if adoptFirst {
		be.completeAdopted(late)
		waitNotInFlight(t, a, sk)
	}
	return append(conn.got(), lateConn.got()...)
}

func sentTexts(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "SendToSession ") {
			out = append(out, c)
		}
	}
	return out
}

// TestSessionSink_LateTextAfterStreamedReply_NothingLost: the common case. The
// turn streamed its reply, so its sink is already delivered when the late text
// arrives; the late text goes out through the same sink, and the suppressed
// FinalText is the reply the user already has. Both texts reach the chat once.
func TestSessionSink_LateTextAfterStreamedReply_NothingLost(t *testing.T) {
	const reply, late = "The build is green.", "Next run: background task finished."
	calls := sentTexts(runInjectedTurnWithLateText(t, reply, reply, late, false))
	if countContaining(calls, reply) != 1 || countContaining(calls, late) != 1 {
		t.Fatalf("sent = %q, want the reply and the late text once each", calls)
	}
}

// TestSessionSink_NextRunAdoptedBeforeItsText_UnstreamedFinalKept is the
// production ccstream path for the same window. With session-state events on
// (foci sets CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS at launch), CC's next run
// opens with a running edge, and ccstream adopts it (OpenAutonomousTurn,
// which registers the run's own sink) synchronously on the reader goroutine
// BEFORE the run's first text. So the next run's text goes to the run's own
// sink, the finished turn's SessionSink never sees it, and even an unstreamed
// final reply is delivered.
func TestSessionSink_NextRunAdoptedBeforeItsText_UnstreamedFinalKept(t *testing.T) {
	const final, late = "Error: CC process exited unexpectedly: signal: killed", "Next run: background task finished."
	calls := sentTexts(runInjectedTurnWithLateText(t, "", final, late, true))
	if countContaining(calls, final) != 1 {
		t.Errorf("the turn's final reply %q was not delivered once; sent = %q", final, calls)
	}
	if countContaining(calls, late) != 1 {
		t.Errorf("the next run's text %q was not delivered once; sent = %q", late, calls)
	}
}
