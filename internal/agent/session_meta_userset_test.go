package agent

import (
	"context"
	"sync"
	"testing"

	"foci/internal/delegator"
	"foci/internal/provider"
)

const (
	staleModel = "claude-old-model"
	userModel  = "claude-user-choice"
)

// inFlightProbeBackend reports a fixed in-flight state and lets a test act at
// the moment the state is read. SendControl stands for CC's set_model wait.
type inFlightProbeBackend struct {
	*mockSetModelBackend
	inFlight  bool
	onProbe   func()
	onControl func()
}

func (b *inFlightProbeBackend) IsTurnInFlight() bool {
	if b.onProbe != nil {
		b.onProbe()
	}
	return b.inFlight
}

func (b *inFlightProbeBackend) SendControl(ctx context.Context, req delegator.ControlRequest) error {
	if b.onControl != nil {
		b.onControl()
	}
	return b.mockSetModelBackend.SendControl(ctx, req)
}

// startTurn loads the session meta as the post-turn path will find it, for a
// turn that reports the given model when it ends.
func startTurn(a *Agent, tr *DelegatedTransport, sk, finalModel string) (*TurnState, func()) {
	ts := NewTurnState(context.Background(), sk, []string{"hi"}, nil)
	tr.LoadSessionMeta(ts)
	ts.FinalModel = finalModel
	ts.FinalUsage = &provider.Usage{InputTokens: 1}
	var once sync.Once
	return ts, func() { once.Do(func() { tr.UpdateSessionMeta(ts) }) }
}

// A turn that began before /model reports the model it ran on. Wherever that
// report lands relative to the switch, the user's choice stands (#2914).
func TestSetModel_StaleTurnEndDoesNotOverwriteChoice(t *testing.T) {
	t.Parallel()
	for name, wire := range map[string]func(b *inFlightProbeBackend, endTurn func()){
		// The ticket's case: the turn is in flight when the model is written and
		// ends before SetModel acts on that.
		"ends between the model write and the in-flight check": func(b *inFlightProbeBackend, endTurn func()) {
			b.inFlight = true
			b.onProbe = endTurn
		},
		// The backend has seen the turn's result, so it no longer reports the
		// turn in flight, but the post-turn path has not recorded it yet.
		"result seen, post-turn path still to run": func(b *inFlightProbeBackend, endTurn func()) {
			b.inFlight = false
		},
		"ends after SetModel returns, still in flight during it": func(b *inFlightProbeBackend, endTurn func()) {
			b.inFlight = true
		},
		"ends during the set_model wait": func(b *inFlightProbeBackend, endTurn func()) {
			b.inFlight = true
			b.onControl = endTurn
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			be := &inFlightProbeBackend{mockSetModelBackend: &mockSetModelBackend{}}
			a, sk := setupAgentWithSetModelBackend(be)
			tr := &DelegatedTransport{sharedTurnOps{agent: a}}
			a.SetSessionModel(sk, staleModel, "", "", nil)

			_, endTurn := startTurn(a, tr, sk, staleModel)
			wire(be, endTurn)

			if err := a.SetModel(context.Background(), sk, userModel, "", "", nil, userModel); err != nil {
				t.Fatalf("SetModel: %v", err)
			}
			endTurn() // a no-op if the wiring above already ended it

			if got := a.SessionModel(sk); got != userModel {
				t.Errorf("SessionModel = %q, want the user's %q", got, userModel)
			}
		})
	}
}

// Control: the guard costs nothing once the stale turn is over. A turn that
// began after /model still records what the backend resolved the choice to.
func TestSetModel_LaterTurnResolvesAlias(t *testing.T) {
	t.Parallel()
	for name, inFlight := range map[string]bool{"no turn in flight at /model": false, "turn in flight at /model": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			be := &inFlightProbeBackend{mockSetModelBackend: &mockSetModelBackend{}, inFlight: inFlight}
			a, sk := setupAgentWithSetModelBackend(be)
			tr := &DelegatedTransport{sharedTurnOps{agent: a}}
			a.SetSessionModel(sk, staleModel, "", "", nil)

			_, endOld := startTurn(a, tr, sk, staleModel)
			if err := a.SetModel(context.Background(), sk, "sonnet", "", "", nil, "sonnet"); err != nil {
				t.Fatalf("SetModel: %v", err)
			}
			endOld()
			if got := a.SessionModel(sk); got != "sonnet" {
				t.Fatalf("after the stale turn: SessionModel = %q, want %q", got, "sonnet")
			}

			const resolved = "claude-sonnet-4-6"
			_, endNew := startTurn(a, tr, sk, resolved)
			endNew()
			if got := a.SessionModel(sk); got != resolved {
				t.Errorf("after a later turn: SessionModel = %q, want the resolved %q", got, resolved)
			}
		})
	}
}

// A turn with no model change around it records the backend's model.
func TestUpdateSessionMeta_NoSwitch_RecordsFinalModel(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	const sk = "test/noswitch"
	a.SetSessionModel(sk, "sonnet", "", "", nil)

	_, end := startTurn(a, tr, sk, "claude-sonnet-4-6")
	end()
	if got := a.SessionModel(sk); got != "claude-sonnet-4-6" {
		t.Errorf("SessionModel = %q, want the backend's model", got)
	}
}
