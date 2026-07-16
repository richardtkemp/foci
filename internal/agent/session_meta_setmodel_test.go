package agent

import (
	"context"
	"errors"
	"testing"

	"foci/internal/delegator"
)

// mockSetModelBackend is a delegator.Delegator whose SendControl reports a
// configurable outcome for delegator.SetModelRequest — used to prove
// Agent.SetModel only persists session metadata once the backend confirms.
type mockSetModelBackend struct {
	delegator.Delegator // embed: unused methods panic if called

	err      error // returned by SendControl for SetModelRequest
	gotModel string
}

func (m *mockSetModelBackend) SendControl(ctx context.Context, req delegator.ControlRequest) error {
	if set, ok := req.(*delegator.SetModelRequest); ok {
		m.gotModel = set.Model
		return m.err
	}
	return nil
}

func (m *mockSetModelBackend) IsRunning() bool      { return true }
func (m *mockSetModelBackend) IsTurnInFlight() bool { return false }

type mockResolvingModelBackend struct {
	*mockSetModelBackend
	resolution delegator.ModelResolution
	resolveErr error
}

func (m *mockResolvingModelBackend) ResolveModel(context.Context, string) (delegator.ModelResolution, error) {
	return m.resolution, m.resolveErr
}

func setupAgentWithSetModelBackend(be delegator.Delegator) (*Agent, string) {
	const sk = "test/setmodel"
	dm := &DelegatedManager{
		backends: map[string]*managedBackend{sk: {be: be}},
	}
	return &Agent{DelegatedManager: dm}, sk
}

// TestSetModel_RejectedByBackend_MetadataUnchanged proves that when the
// backend rejects a model switch (e.g. CC reports an unrecognized model id),
// Agent.SetModel leaves the session's recorded model untouched instead of
// optimistically claiming the switch happened.
func TestSetModel_RejectedByBackend_MetadataUnchanged(t *testing.T) {
	be := &mockSetModelBackend{err: errors.New(`Model "fake" is not a recognized model id`)}
	ag, sk := setupAgentWithSetModelBackend(be)
	ag.SetSessionModel(sk, "claude-sonnet-4-6", "", "", nil)

	err := ag.SetModel(context.Background(), sk, "fake", "", "", nil, "fake")
	if err == nil {
		t.Fatal("SetModel: got nil error, want the backend's rejection surfaced")
	}

	if got := ag.SessionModel(sk); got != "claude-sonnet-4-6" {
		t.Errorf("SessionModel after rejected switch = %q, want unchanged %q", got, "claude-sonnet-4-6")
	}
}

// TestSetModel_ConfirmedByBackend_MetadataUpdated proves the mirror case:
// once the backend confirms, the new model is recorded.
func TestSetModel_ConfirmedByBackend_MetadataUpdated(t *testing.T) {
	be := &mockSetModelBackend{err: nil}
	ag, sk := setupAgentWithSetModelBackend(be)
	ag.SetSessionModel(sk, "claude-sonnet-4-6", "", "", nil)

	if err := ag.SetModel(context.Background(), sk, "claude-opus-4-8", "", "", nil, "opus"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}

	if got := ag.SessionModel(sk); got != "claude-opus-4-8" {
		t.Errorf("SessionModel after confirmed switch = %q, want %q", got, "claude-opus-4-8")
	}
}

// TestSetModel_ResolvedByBackendPersistsCanonicalModel proves a catalogue alias
// is converted before control delivery and foci records the backend's canonical
// developer-qualified id rather than the user's shorthand.
func TestSetModel_ResolvedByBackendPersistsCanonicalModel(t *testing.T) {
	base := &mockSetModelBackend{}
	be := &mockResolvingModelBackend{
		mockSetModelBackend: base,
		resolution: delegator.ModelResolution{
			BackendModel: "gpt-5.6-luna",
			Model:        "codex/gpt-5.6-luna",
		},
	}
	ag, sk := setupAgentWithSetModelBackend(be)
	if err := ag.SetModel(context.Background(), sk, "luna", "", "", nil, "luna"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if base.gotModel != "gpt-5.6-luna" {
		t.Errorf("backend request model = %q", base.gotModel)
	}
	if got := ag.SessionModel(sk); got != "codex/gpt-5.6-luna" {
		t.Errorf("persisted model = %q", got)
	}
}
