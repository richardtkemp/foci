package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
)

// InjectsRecorded snapshots the mock backend's recorded ImmediateInject calls.
func (m *mockBackendDT) InjectsRecorded() []delegator.Inject {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]delegator.Inject, len(m.injects))
	copy(out, m.injects)
	return out
}

// setTurnInFlight toggles the IsTurnInFlight() result under mu so Enqueue's
// routing (another goroutine) sees the change safely — the adopted-autonomous-
// run shape: the backend has a turn in flight while the inbox's own turnActive
// stays false (#1274).
func (m *mockBackendDT) setTurnInFlight(v bool) {
	m.mu.Lock()
	m.turnInFlight = v
	m.mu.Unlock()
}

// runUserInputTurn drives RunInference for an ask-answer turn: trigger
// ask_grader (so via=ask-grader is unchanged) with the user-input ctx marker.
func runUserInputTurn(t *testing.T, a *Agent, texts []string) *TurnState {
	t.Helper()
	// A deadline turns a regression that sends the answer down the system
	// retry loop into a prompt failure instead of a hang (#2435).
	base, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	ctx := WithTrigger(WithUserInput(base), "ask_grader")
	ts := NewTurnState(ctx, "test/s", texts, nil)
	ts.Trigger = "ask_grader"
	ts.Prompt = texts[0]
	ts.StartedAt = time.Now()
	if err := (&DelegatedTransport{sharedTurnOps{agent: a}}).RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}
	return ts
}

// TestRunInference_UserInput_BeginsAsSourceUser pins requirement 3 on the
// delegated backend: an ask answer at idle begins as SourceUser, so the
// backend's Pending()>0 refusal (which applies to SourceSystem begins) never
// applies to it.
func TestRunInference_UserInput_BeginsAsSourceUser(t *testing.T) {
	t.Parallel()
	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}

	runUserInputTurn(t, a, []string{"the answer"})

	if n := len(be.InjectsRecorded()); n != 1 {
		t.Fatalf("injects = %d, want 1", n)
	}
	if src := be.InjectsRecorded()[0].Source; src != delegator.SourceUser {
		t.Errorf("Source = %v, want SourceUser — an ask answer takes the user path (#2233)", src)
	}
}

// TestRunInference_UserInput_FoldsAsSourceUserWhenInFlight pins the in-flight
// half: with a turn running, the answer folds in as a SourceUser follow-up.
func TestRunInference_UserInput_FoldsAsSourceUserWhenInFlight(t *testing.T) {
	t.Parallel()
	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl", turnInFlight: true}
	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}

	runUserInputTurn(t, a, []string{"fold me"})

	if n := len(be.InjectsRecorded()); n != 1 {
		t.Fatalf("injects = %d, want 1", n)
	}
	if src := be.InjectsRecorded()[0].Source; src != delegator.SourceUser {
		t.Errorf("Source = %v, want SourceUser (in-flight fold, never a system begin)", src)
	}
}

// systemRefusingBackend records any SourceSystem inject it is handed and
// refuses it with ErrTurnInFlight (also clearing the flag so a broken caller
// proceeds instead of spinning), letting a test prove an ask answer was never
// retried as system input.
type systemRefusingBackend struct {
	mockBackendDT
	mu         sync.Mutex
	systemSeen bool
}

func (b *systemRefusingBackend) ImmediateInject(_ context.Context, inj delegator.Inject) error {
	if inj.Source == delegator.SourceSystem {
		b.mu.Lock()
		b.systemSeen = true
		b.mu.Unlock()
		b.turnInFlight = false // let a broken caller proceed; the flag is the verdict
		return delegator.ErrTurnInFlight
	}
	return b.mockBackendDT.ImmediateInject(nil, inj)
}

// TestRunInference_UserInput_NeverRetriedAsSystem: a user-input turn with a
// backend that refuses every SourceSystem begin with ErrTurnInFlight still
// dispatches — via the SourceUser fold — and never produces a SourceSystem
// inject.
func TestRunInference_UserInput_NeverRetriedAsSystem(t *testing.T) {
	t.Parallel()
	be := &systemRefusingBackend{}
	be.sessionFile = "/tmp/s.jsonl"
	be.turnInFlight = true
	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}

	done := make(chan struct{})
	go func() {
		runUserInputTurn(t, a, []string{"never system"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunInference never returned — the answer was retried as system input against a refusing backend")
	}
	be.mu.Lock()
	seen := be.systemSeen
	be.mu.Unlock()
	if seen {
		t.Fatal("an ask answer was dispatched as SourceSystem — it must fold or begin as SourceUser only (#2233)")
	}
}

// questionBackend has a pending AskUserQuestion, so a wrong routing would
// consume the turn's text as the prompt's answer.
type questionBackend struct {
	mockBackendDT
	responded atomic.Bool
}

func (b *questionBackend) HasPendingQuestion() string { return "q-1" }
func (b *questionBackend) RespondToQuestion(string, string) error {
	b.responded.Store(true)
	return nil
}
func (b *questionBackend) CancelQuestion(string) error { return nil }

// TestRunInference_UserInput_DoesNotAnswerBackendPrompt pins requirement 3:
// an ask answer must never answer a CC AskUserQuestion or elicitation prompt
// — the answer belongs to the ask; the prompt keeps waiting.
func TestRunInference_UserInput_DoesNotAnswerBackendPrompt(t *testing.T) {
	t.Parallel()
	be := &questionBackend{}
	be.sessionFile = "/tmp/s.jsonl"
	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}

	runUserInputTurn(t, a, []string{"the answer"})

	if be.responded.Load() {
		t.Fatal("an ask answer was consumed as a pending AskUserQuestion's answer — it must never answer a backend prompt (#2233)")
	}
	if n := len(be.InjectsRecorded()); n != 1 {
		t.Fatalf("injects = %d, want the turn dispatched normally", n)
	}
}

// TestRunInference_NonUserInject_StillSourceSystem is the characterisation
// control: the same trigger WITHOUT the user-input marker keeps the system
// dispatch (this is the shape ask expiry notices keep, #2233).
func TestRunInference_NonUserInject_StillSourceSystem(t *testing.T) {
	t.Parallel()
	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}

	ctx := WithTrigger(context.Background(), "ask_grader")
	ts := NewTurnState(ctx, "test/s", []string{"expiry notice"}, nil)
	ts.Trigger = "ask_grader"
	ts.Prompt = "expiry notice"
	ts.StartedAt = time.Now()
	if err := (&DelegatedTransport{sharedTurnOps{agent: a}}).RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}

	if n := len(be.InjectsRecorded()); n != 1 {
		t.Fatalf("injects = %d, want 1", n)
	}
	if src := be.InjectsRecorded()[0].Source; src != delegator.SourceSystem {
		t.Errorf("Source = %v, want SourceSystem — only the user-input marker widens folding (#2233)", src)
	}
}
