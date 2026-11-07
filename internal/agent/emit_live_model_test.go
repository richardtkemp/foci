package agent

import (
	"context"
	"slices"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/turnevent"
)

// TestEmitLiveModel proves a delegated turn tells its sink the model its backend
// reports, and stays silent when the backend has none or cannot say (#2879).
func TestEmitLiveModel(t *testing.T) {
	t.Parallel()
	emitted := func(be delegator.Delegator) []string {
		var got []string
		emitLiveModel(turnevent.WithSink(context.Background(), modelRecordingSink{got: &got}), be)
		return got
	}
	if got := emitted(&liveModelFake{running: true, model: "codex/gpt-6.1-sol"}); !slices.Equal(got, []string{"codex/gpt-6.1-sol"}) {
		t.Errorf("reporting backend: emitted %v", got)
	}
	if got := emitted(&liveModelFake{running: true}); len(got) != 0 {
		t.Errorf("backend with no model yet: emitted %v", got)
	}
	if got := emitted(plainFake{}); len(got) != 0 {
		t.Errorf("backend that cannot say: emitted %v", got)
	}
}

type modelRecordingSink struct{ got *[]string }

func (s modelRecordingSink) Emit(_ context.Context, ev turnevent.Event) {
	if m, ok := ev.(turnevent.ModelKnown); ok {
		*s.got = append(*s.got, m.Model)
	}
}

func (modelRecordingSink) DeliversToPlatform() bool { return false }

type liveModelMockDT struct {
	*mockBackendDT
	model string
}

func (b liveModelMockDT) LiveModel() string { return b.model }

// TestRunInference_EmitsLiveModel proves the event reaches the sink a spawn's
// turn runs under, before the turn completes (#2879).
func TestRunInference_EmitsLiveModel(t *testing.T) {
	t.Parallel()
	var got []string
	var atBegin int
	be := liveModelMockDT{model: "codex/gpt-6.1-sol", mockBackendDT: &mockBackendDT{
		sendToPaneFn: func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
			atBegin = len(got)
			handler.OnTurnComplete(&delegator.TurnResult{Text: "ok"})
			return nil, nil
		},
	}}
	a := &Agent{Model: "", DelegatedManager: newMockDelegatedManager(t, be)}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(turnevent.WithSink(context.Background(), modelRecordingSink{got: &got}), "test/s", []string{"hello"}, nil)
	ts.Prompt = "hello"
	ts.StartedAt = time.Now()
	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}
	if atBegin != 1 || !slices.Equal(got, []string{"codex/gpt-6.1-sol"}) {
		t.Errorf("emitted %v, %d of them before the turn began; want the model, once, before", got, atBegin)
	}
}
