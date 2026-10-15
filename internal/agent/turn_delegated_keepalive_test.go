package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"foci/internal/delegator"
)

// runSystemTurn drives RunInference for a system-trigger turn with no
// user-input marker.
func runSystemTurn(t *testing.T, a *Agent, trigger string) *TurnState {
	t.Helper()
	ctx := WithTrigger(context.Background(), trigger)
	ts := NewTurnState(ctx, "test/s", []string{"prompt"}, nil)
	ts.Trigger = trigger
	ts.Prompt = "prompt"
	ts.StartedAt = time.Now()
	if err := (&DelegatedTransport{sharedTurnOps{agent: a}}).RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}
	return ts
}

// TestRunInference_KeepaliveSetsPendingWorkExempt pins the trigger-keyed
// exemption (#2233): a keepalive turn's SourceSystem begin carries
// PendingWorkExempt, so the backend may start it while background work is
// pending — a two-hour hold would defeat the keepalive's whole purpose.
func TestRunInference_KeepaliveSetsPendingWorkExempt(t *testing.T) {
	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}

	runSystemTurn(t, a, TriggerKeepalive)

	injects := be.InjectsRecorded()
	if n := len(injects); n != 1 {
		t.Fatalf("injects = %d, want 1", n)
	}
	if !injects[0].PendingWorkExempt {
		t.Error("keepalive begin carried PendingWorkExempt=false — identified by trigger, it must be exempt (#2233)")
	}
	if src := injects[0].Source; src != delegator.SourceSystem {
		t.Errorf("Source = %v, want SourceSystem (a keepalive is still a system turn)", src)
	}
}

// TestRunInference_OtherSystemTriggersNotExempt is the characterisation
// control: every other system trigger keeps the pending-work refusal — no
// exemption, exactly as before.
func TestRunInference_OtherSystemTriggersNotExempt(t *testing.T) {
	for _, trigger := range []string{"reflection", "scheduled_wake", "user", "webhook", "ask_grader"} {
		t.Run(trigger, func(t *testing.T) {
			be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
			mgr := newMockDelegatedManager(t, be)
			a := &Agent{Model: "test-model", DelegatedManager: mgr}

			runSystemTurn(t, a, trigger)

			injects := be.InjectsRecorded()
			if n := len(injects); n != 1 {
				t.Fatalf("injects = %d, want 1", n)
			}
			if injects[0].PendingWorkExempt {
				t.Errorf("trigger %q wrongly carried PendingWorkExempt — only keepalive is exempt", trigger)
			}
		})
	}
}

// TestRunInference_KeepaliveSkippedWhenBeginRefused pins the N1 rule (#2233
// review): a keepalive whose exclusive begin the backend refuses
// (ErrTurnInFlight) returns ErrKeepaliveSkipped after ONE attempt instead of
// waiting for the turn — while another system trigger keeps waiting and
// retrying (characterisation).
func TestRunInference_KeepaliveSkippedWhenBeginRefused(t *testing.T) {
	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be.setTurnInFlight(true)
	waits := 0
	be.waitForTurnFn = func(ctx context.Context) error {
		waits++
		be.setTurnInFlight(false) // the turn ends; a retry would now begin
		return nil
	}
	a := &Agent{Model: "test-model", DelegatedManager: newMockDelegatedManager(t, be)}

	ctx := WithTrigger(context.Background(), TriggerKeepalive)
	ts := NewTurnState(ctx, "test/s", []string{"prompt"}, nil)
	ts.Trigger = TriggerKeepalive
	ts.Prompt = "prompt"
	ts.StartedAt = time.Now()
	err := (&DelegatedTransport{sharedTurnOps{agent: a}}).RunInference(ts)
	if !errors.Is(err, ErrKeepaliveSkipped) {
		t.Fatalf("RunInference = %v, want ErrKeepaliveSkipped", err)
	}
	if waits != 0 || len(be.InjectsRecorded()) != 1 {
		t.Fatalf("waits=%d injects=%d, want 0 waits and exactly 1 begin attempt", waits, len(be.InjectsRecorded()))
	}

	// Control: a wake waits for the turn and retries.
	be2 := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be2.setTurnInFlight(true)
	be2.waitForTurnFn = func(ctx context.Context) error {
		be2.setTurnInFlight(false)
		return nil
	}
	a2 := &Agent{Model: "test-model", DelegatedManager: newMockDelegatedManager(t, be2)}
	runSystemTurn(t, a2, "scheduled_wake")
	if n := len(be2.InjectsRecorded()); n != 2 {
		t.Fatalf("wake begin attempts = %d, want 2 (refused, then retried after the turn)", n)
	}
}
