package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
)

// #3072: a follow-up that opencode rejects at a natural session.idle must not
// leave a phantom active turn, and must not lose the buffered text.

// rejectPrompts makes /prompt_async answer HTTP 500 while reject is true.
func rejectPrompts(rec *recordingHandler, reject *atomic.Bool) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.onRequest = func(path string) int {
		if path == "/session/sess-inject/prompt_async" && reject.Load() {
			return http.StatusInternalServerError
		}
		return 0
	}
}

func steerBufSnapshot(b *Backend) []string {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	return append([]string(nil), b.steerBuf...)
}

func lastPromptText(t *testing.T, rec *recordingHandler) string {
	t.Helper()
	req, ok := rec.lastPath("/session/sess-inject/prompt_async")
	if !ok {
		t.Fatal("no /prompt_async recorded")
	}
	var body struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("decode prompt body: %v", err)
	}
	if len(body.Parts) == 0 {
		t.Fatal("prompt body has no parts")
	}
	return body.Parts[0].Text
}

func TestNaturalIdleFlush_Rejected_NoPhantomTurn_TextKept(t *testing.T) {
	rec := &recordingHandler{}
	var reject atomic.Bool
	reject.Store(true)
	rejectPrompts(rec, &reject)
	b := newReadyBackend(t, rec)

	// A turn is running; the user sends a follow-up, which queues.
	b.beginTurn(&delegator.TurnEvents{})
	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceUser, Text: "follow-up",
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	// The turn ends on its own; the flush POST is rejected with HTTP 500.
	b.onSessionIdle(b.sessionID)

	if got := rec.countPath("/session/sess-inject/prompt_async"); got != 1 {
		t.Fatalf("/prompt_async POSTs = %d, want 1 (the rejected flush)", got)
	}
	if b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = true after a rejected flush: phantom turn (#3072)")
	}
	if got := steerBufSnapshot(b); len(got) != 1 || got[0] != "follow-up" {
		t.Errorf("steerBuf = %q after a rejected flush, want [follow-up] (text lost, #3072)", got)
	}

	// A later system inject must begin a turn, not wait behind a phantom.
	reject.Store(false)
	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSystem, Text: "cron", Turn: &delegator.TurnEvents{},
	}); err != nil {
		t.Fatalf("system inject after rejected flush: %v (want a fresh turn)", err)
	}

	// When that turn ends, the kept follow-up is retried.
	b.onSessionIdle(b.sessionID)
	if got := rec.countPath("/session/sess-inject/prompt_async"); got != 3 {
		t.Fatalf("/prompt_async POSTs = %d, want 3 (rejected, system, retry)", got)
	}
	if got := lastPromptText(t, rec); got != "follow-up" {
		t.Errorf("retried text = %q, want %q", got, "follow-up")
	}
	if got := steerBufSnapshot(b); len(got) != 0 {
		t.Errorf("steerBuf = %q after the retry, want empty", got)
	}
}

// A message queued while the rejected POST is open stays after the kept one.
func TestNaturalIdleFlush_Rejected_KeepsOrderWithLaterMessages(t *testing.T) {
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)
	rec.mu.Lock()
	rec.onRequest = func(path string) int {
		if path != "/session/sess-inject/prompt_async" {
			return 0
		}
		// The flush began its turn, so this inject queues behind it.
		if err := b.ImmediateInject(context.Background(), delegator.Inject{
			Source: delegator.SourceUser, Text: "second",
		}); err != nil {
			t.Errorf("Inject during POST: %v", err)
		}
		return http.StatusInternalServerError
	}
	rec.mu.Unlock()

	b.beginTurn(&delegator.TurnEvents{})
	b.turnMu.Lock()
	b.steerBuf = []string{"first"}
	b.turnMu.Unlock()

	b.onSessionIdle(b.sessionID)

	if b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = true after a rejected flush")
	}
	if got := steerBufSnapshot(b); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("steerBuf = %q, want [first second]", got)
	}
}

// The abort-drain flush keeps a rejected steer too, and leaves no turn.
func TestAbortDrainFlush_Rejected_TextKept(t *testing.T) {
	rec := &recordingHandler{}
	var reject atomic.Bool
	reject.Store(true)
	rejectPrompts(rec, &reject)
	b := newReadyBackend(t, rec)
	b.abortDrainTimeout = time.Hour

	b.beginTurn(&delegator.TurnEvents{})
	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer, Text: "steer",
	}); err != nil {
		t.Fatalf("Inject steer: %v", err)
	}
	b.onSessionError(b.sessionID, &MessageError{Name: ErrMessageAborted})
	b.onSessionIdle(b.sessionID)
	b.onSessionIdle(b.sessionID)

	if got := rec.countPath("/session/sess-inject/prompt_async"); got != 1 {
		t.Fatalf("/prompt_async POSTs = %d, want 1", got)
	}
	if b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = true after a rejected abort-drain flush")
	}
	if got := steerBufSnapshot(b); len(got) != 1 || got[0] != "steer" {
		t.Errorf("steerBuf = %q, want [steer]", got)
	}
}
