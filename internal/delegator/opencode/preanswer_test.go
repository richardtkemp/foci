package opencode

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"foci/internal/delegator"
)

// preAnswerTestBackend is a handler-test Backend whose server is an httptest
// stub answering POST /prompt_async with status, recording each prompt's text.
func preAnswerTestBackend(t *testing.T, status int) (*Backend, func() []string) {
	t.Helper()
	b := newHandlerTestBackend(t)
	var mu sync.Mutex
	var prompts []string
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/prompt_async") {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		}
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		if len(body.Parts) > 0 {
			prompts = append(prompts, body.Parts[0].Text)
		}
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(hs.Close)
	b.server = &Server{baseURL: hs.URL, http: hs.Client(), agentID: "test-agent", sessions: map[string]*Backend{}, running: true}
	return b, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), prompts...)
	}
}

// armPreAnswerTurn installs a turn whose gate returns followUp once (then ""),
// the shape the agent's preAnswerNudgeFunc has. It returns the completion
// capture and the gate's call count.
func armPreAnswerTurn(b *Backend, followUp string) (completed func() *delegator.TurnResult, gateCalls func() int) {
	var mu sync.Mutex
	var done *delegator.TurnResult
	calls := 0
	b.beginTurn(&delegator.TurnEvents{
		OnTurnComplete: func(r *delegator.TurnResult) {
			mu.Lock()
			done = r
			mu.Unlock()
		},
		PreAnswerNudgeFunc: func(*delegator.TurnResult) string {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls == 1 {
				return followUp
			}
			return ""
		},
	})
	return func() *delegator.TurnResult {
			mu.Lock()
			defer mu.Unlock()
			return done
		}, func() int {
			mu.Lock()
			defer mu.Unlock()
			return calls
		}
}

func setTurnText(b *Backend, s string) {
	b.turnMu.Lock()
	b.turnText.Reset()
	b.turnText.WriteString(s)
	b.turnMu.Unlock()
}

// TestOnSessionIdle_PreAnswerReDispatches proves the pre-answer round trip:
// the first idle sends the gate's follow-up and holds the turn open (same
// TurnEvents, same WaitForTurn channel); the second idle completes the turn
// with the revised answer only. Named as the opencode pre_answer_nudge proof
// in internal/delegator/all/spec_test.go.
func TestOnSessionIdle_PreAnswerReDispatches(t *testing.T) {
	b, prompts := preAnswerTestBackend(t, http.StatusNoContent)
	completed, gateCalls := armPreAnswerTurn(b, "please verify")
	b.turnMu.Lock()
	ch := b.turnResultCh
	b.turnMu.Unlock()

	setTurnText(b, "first answer")
	b.onSessionIdle("sess-test")

	if got := prompts(); len(got) != 1 || got[0] != "please verify" {
		t.Fatalf("prompts sent = %q, want [please verify]", got)
	}
	if completed() != nil {
		t.Fatal("turn completed on the first idle; want it held open for the revision")
	}
	if !b.IsTurnInFlight() {
		t.Fatal("turn not in flight after the re-dispatch")
	}
	b.turnMu.Lock()
	sameCh := b.turnResultCh == ch
	textAfter := b.turnText.String()
	b.turnMu.Unlock()
	if !sameCh {
		t.Error("re-dispatch replaced turnResultCh: a WaitForTurn caller from round 1 would never be signalled")
	}
	if textAfter != "" {
		t.Errorf("turnText after re-dispatch = %q, want reset", textAfter)
	}

	setTurnText(b, "revised answer")
	b.onSessionIdle("sess-test")

	r := completed()
	if r == nil {
		t.Fatal("turn not completed on the second idle")
	}
	if r.Text != "revised answer" {
		t.Errorf("final text = %q, want the revised answer only", r.Text)
	}
	if gateCalls() != 2 {
		t.Errorf("gate called %d times, want 2 (once per round)", gateCalls())
	}
	if b.IsTurnInFlight() {
		t.Error("turn still in flight after completion")
	}
	select {
	case <-ch:
	default:
		t.Error("round-1 WaitForTurn channel not signalled at completion")
	}
}

// TestOnSessionIdle_PreAnswerSendFailureCompletesFirstRound: when the
// follow-up POST fails, no second idle will ever arrive, so the turn must
// complete now with the first-round answer instead of waiting forever.
func TestOnSessionIdle_PreAnswerSendFailureCompletesFirstRound(t *testing.T) {
	b, prompts := preAnswerTestBackend(t, http.StatusInternalServerError)
	completed, _ := armPreAnswerTurn(b, "please verify")

	setTurnText(b, "first answer")
	b.onSessionIdle("sess-test")

	if got := prompts(); len(got) != 1 {
		t.Fatalf("prompts attempted = %q, want exactly the one failed follow-up", got)
	}
	r := completed()
	if r == nil {
		t.Fatal("turn left waiting for an idle that never comes after the follow-up send failed")
	}
	if r.Text != "first answer" {
		t.Errorf("final text = %q, want the first-round answer", r.Text)
	}
	if b.IsTurnInFlight() {
		t.Error("turn still in flight after the failed re-dispatch")
	}
}

// TestOnSessionIdle_PreAnswerSkippedDuringAbortDrain: an idle from a steer's
// abort burst is not a final answer, so the gate must not run and re-send.
func TestOnSessionIdle_PreAnswerSkippedDuringAbortDrain(t *testing.T) {
	b, prompts := preAnswerTestBackend(t, http.StatusNoContent)
	_, gateCalls := armPreAnswerTurn(b, "please verify")
	b.turnMu.Lock()
	b.aborting = true
	b.turnMu.Unlock()
	t.Cleanup(func() {
		b.turnMu.Lock()
		b.disarmAbortLocked()
		b.turnMu.Unlock()
	})

	setTurnText(b, "partial")
	b.onSessionIdle("sess-test")

	if n := gateCalls(); n != 0 {
		t.Errorf("gate called %d times during an abort drain, want 0", n)
	}
	if got := prompts(); len(got) != 0 {
		t.Errorf("prompts sent during an abort drain = %q, want none", got)
	}
}
