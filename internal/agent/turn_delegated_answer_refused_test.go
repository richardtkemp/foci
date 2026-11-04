package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
)

// refusingQuestionBackend is a backend whose AskUserQuestion refuses the typed
// answer. stillPending says whether the prompt survives the refusal.
type refusingQuestionBackend struct {
	mockBackendDT
	pending      string
	stillPending bool
	err          error
	answered     chan string
}

func (b *refusingQuestionBackend) HasPendingQuestion() string  { return b.pending }
func (b *refusingQuestionBackend) CancelQuestion(string) error { return nil }
func (b *refusingQuestionBackend) RespondToQuestion(_, text string) error {
	if b.answered != nil {
		b.answered <- text
	}
	if !b.stillPending {
		b.pending = ""
	}
	return b.err
}

// refusingElicitationBackend is the elicitation twin.
type refusingElicitationBackend struct {
	mockBackendDT
	pending string
	err     error
}

func (b *refusingElicitationBackend) HasPendingElicitation() string { return b.pending }
func (b *refusingElicitationBackend) RespondToElicitation(string, string) error {
	return b.err
}

// runInterruptibleTurn runs an interactive RunInference with a hang guard. A
// worker parked behind the prompt fails the test instead of hanging it.
func runInterruptibleTurn(t *testing.T, a *Agent, text string) *TurnState {
	t.Helper()
	ctx, cancel := context.WithCancel(interactiveTestCtx())
	defer cancel()
	ts := NewTurnState(ctx, "test/s", []string{text}, nil)
	ts.Prompt = text
	ts.Trigger = TriggerFromContext(ctx)
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	done := make(chan error, 1)
	go func() { done <- tr.RunInference(ts) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunInference: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunInference is parked behind the waiting prompt: the user can no longer answer it by typing")
	}
	return ts
}

// TestRunInference_RefusedTypedAnswer_PromptStillWaiting pins #2762: when the
// backend refuses a typed answer and its prompt is still waiting (a bad value
// for an integer field, a failed opencode POST), the user is told, and the
// worker stays free so that the next typed message can answer the prompt.
func TestRunInference_RefusedTypedAnswer_PromptStillWaiting(t *testing.T) {
	t.Parallel()
	refusal := errors.New(`invalid integer "abc"`)
	for _, tc := range []struct {
		name string
		be   delegator.Delegator
	}{
		{"question", &refusingQuestionBackend{pending: "req-1", stillPending: true, err: refusal}},
		{"elicitation", &refusingElicitationBackend{pending: "req-1", err: refusal}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mgr := newMockDelegatedManager(t, tc.be)
			mgr.SetPermissionPending("test/s", true)
			notices := make(chan [2]string, 1)
			a := &Agent{Model: "test-model", DelegatedManager: mgr}
			a.PromptAnswerRefusedNotifyFunc = func(sk, text string) { notices <- [2]string{sk, text} }

			ts := runInterruptibleTurn(t, a, "abc")

			select {
			case n := <-notices:
				if n[0] != "test/s" {
					t.Errorf("notice went to session %q, want test/s", n[0])
				}
				if !strings.Contains(n[1], refusal.Error()) {
					t.Errorf("notice %q does not name the refusal", n[1])
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the user was not told that the answer was refused")
			}
			if ts.inputDelivered {
				t.Error("inputDelivered = true for an answer the backend refused")
			}
			if !ts.foldedFollowUp {
				t.Error("foldedFollowUp = false: this turn's TurnComplete would finalize the running turn's sink")
			}
		})
	}
}

// TestRunInference_RefusedTypedAnswer_PromptGone pins the other arm: the prompt
// is no longer waiting (a button click won the race, or the backend claimed the
// prompt and then failed to write), so the text is ordinary input. It runs as a
// normal turn and is not lost, and no notice claims that a prompt still waits.
func TestRunInference_RefusedTypedAnswer_PromptGone(t *testing.T) {
	t.Parallel()
	var dispatched string
	be := &refusingQuestionBackend{pending: "req-1", err: errors.New("broken pipe")}
	be.sendToPaneFn = func(_ context.Context, prompt string, handler *mockHandler) (*delegator.TurnResult, error) {
		dispatched = prompt
		if handler != nil && handler.OnTurnComplete != nil {
			handler.OnTurnComplete(&delegator.TurnResult{Text: "done"})
		}
		return nil, nil
	}
	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	a.PromptAnswerRefusedNotifyFunc = func(_, text string) { t.Errorf("unexpected notice %q", text) }

	runInterruptibleTurn(t, a, "my answer")

	if dispatched != "my answer" {
		t.Errorf("dispatched = %q, want the text to run as a normal turn", dispatched)
	}
}

// TestRunInference_AcceptedTypedAnswer_NoNotice pins that the normal answer
// path stays silent and still counts as delivered.
func TestRunInference_AcceptedTypedAnswer_NoNotice(t *testing.T) {
	t.Parallel()
	answered := make(chan string, 1)
	be := &refusingQuestionBackend{pending: "req-1", answered: answered}
	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	a.PromptAnswerRefusedNotifyFunc = func(_, text string) { t.Errorf("unexpected notice %q", text) }

	ts := runInterruptibleTurn(t, a, "  yes  ")

	select {
	case got := <-answered:
		if got != "yes" {
			t.Errorf("answer = %q, want the trimmed text", got)
		}
	default:
		t.Fatal("the typed text did not answer the prompt")
	}
	if !ts.inputDelivered || !ts.foldedFollowUp {
		t.Errorf("inputDelivered=%v foldedFollowUp=%v, want both true", ts.inputDelivered, ts.foldedFollowUp)
	}
}
