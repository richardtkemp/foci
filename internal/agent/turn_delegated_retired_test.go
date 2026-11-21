package agent

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
)

// retiringBackendDT is running when the turn fetches it, and stops at the
// begin: it refuses the turn with ErrBackendClosed before sending anything, as
// codex does when its app-server was retired after a codex upgrade (#3233).
type retiringBackendDT struct {
	*mockBackendDT
	running atomic.Bool
	begins  atomic.Int32
}

func (b *retiringBackendDT) IsRunning() bool { return b.running.Load() }

func (b *retiringBackendDT) ImmediateInject(_ context.Context, _ delegator.Inject) error {
	b.begins.Add(1)
	b.running.Store(false)
	return fmt.Errorf("codex: app-server restarting after a codex upgrade: %w", delegator.ErrBackendClosed)
}

// TestRunInference_BeginsAgainWhenBackendStoppedAtBegin proves a turn whose
// backend stops between Get and the begin runs on the respawned backend
// instead of failing (#3233).
func TestRunInference_BeginsAgainWhenBackendStoppedAtBegin(t *testing.T) {
	t.Parallel()
	retiring := &retiringBackendDT{mockBackendDT: &mockBackendDT{}}
	retiring.running.Store(true)
	var freshBegins atomic.Int32
	fresh := &mockBackendDT{
		sendToPaneFn: func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
			freshBegins.Add(1)
			handler.OnTurnComplete(&delegator.TurnResult{Text: "ok"})
			return nil, nil
		},
	}
	var created atomic.Int32
	mgr := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			if created.Add(1) == 1 {
				return retiring, nil
			}
			return fresh, nil
		},
		Spec: mockSpecFor(fresh),
	}
	if _, err := mgr.Get(context.Background(), "test/s"); err != nil {
		t.Fatalf("pre-register backend: %v", err)
	}
	a := &Agent{Model: "", DelegatedManager: mgr}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hello"}, nil)
	ts.Prompt = "hello"
	ts.StartedAt = time.Now()

	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v (the turn failed instead of beginning on the respawned backend)", err)
	}
	if n := retiring.begins.Load(); n != 1 {
		t.Errorf("begins on the stopped backend = %d, want 1", n)
	}
	if n := freshBegins.Load(); n != 1 {
		t.Errorf("begins on the respawned backend = %d, want 1", n)
	}
	if ts.Backend != fresh {
		t.Errorf("ts.Backend is not the respawned backend")
	}
}
