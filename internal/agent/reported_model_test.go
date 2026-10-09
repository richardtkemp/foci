package agent

import (
	"errors"
	"testing"

	"foci/internal/delegator"
)

// liveModelFake is a delegator fake whose running state and live model
// (delegator.LiveModelReporter) the test sets per arm. Unused Delegator
// methods panic via the nil embed (mockPermBackend pattern).
type liveModelFake struct {
	delegator.Delegator

	running bool
	model   string
}

func (f *liveModelFake) IsRunning() bool   { return f.running }
func (f *liveModelFake) LiveModel() string { return f.model }

// plainFake reports running but does not implement LiveModelReporter.
type plainFake struct{ delegator.Delegator }

func (plainFake) IsRunning() bool { return true }

// newReportedModelAgent wires an Agent configured with the alias "opus" and a
// real DelegatedManager whose backends map the test seeds by hand — the
// setupAgentWithMockBackend shape, without the forced entry. NewBackend
// always errors: nothing in this file may create a backend.
func newReportedModelAgent() (*Agent, *DelegatedManager) {
	dm := &DelegatedManager{
		backends:   make(map[string]*managedBackend),
		NewBackend: func() (delegator.Delegator, error) { return nil, errors.New("must not create a backend") },
		StartOpts:  delegator.StartOptions{},
	}
	return &Agent{Model: "opus", DelegatedManager: dm}, dm
}

// TestDelegatedManager_BackendLiveModel_ReportedByRunningBackend proves the
// manager lookup hands back the RUNNING backend's live model — the resolved,
// provider-qualified id — without creating or starting anything.
func TestDelegatedManager_BackendLiveModel_ReportedByRunningBackend(t *testing.T) {
	t.Parallel()

	_, dm := newReportedModelAgent()
	dm.backends["test/s"] = &managedBackend{be: &liveModelFake{running: true, model: "claude/claude-opus-5-5"}}

	if got := dm.BackendLiveModel("test/s"); got != "claude/claude-opus-5-5" {
		t.Errorf("BackendLiveModel = %q, want %q", got, "claude/claude-opus-5-5")
	}
	if n := len(dm.backends); n != 1 {
		t.Errorf("backend count = %d after the lookup, want 1 (a read must not create a backend)", n)
	}
}

// TestDelegatedManager_BackendLiveModel_EmptyWhenUnavailable pins the empty
// arms: no managed backend for the key, a backend that is not running, and a
// backend without the LiveModelReporter interface all report "".
func TestDelegatedManager_BackendLiveModel_EmptyWhenUnavailable(t *testing.T) {
	t.Parallel()

	t.Run("no managed backend for the key", func(t *testing.T) {
		t.Parallel()
		_, dm := newReportedModelAgent()
		if got := dm.BackendLiveModel("test/absent"); got != "" {
			t.Errorf("BackendLiveModel with no managed backend = %q, want \"\"", got)
		}
	})

	t.Run("backend not running", func(t *testing.T) {
		t.Parallel()
		_, dm := newReportedModelAgent()
		dm.backends["test/s"] = &managedBackend{be: &liveModelFake{running: false, model: "claude/claude-opus-5-5"}}
		if got := dm.BackendLiveModel("test/s"); got != "" {
			t.Errorf("BackendLiveModel with a stopped backend = %q, want \"\"", got)
		}
	})

	t.Run("backend without the interface", func(t *testing.T) {
		t.Parallel()
		_, dm := newReportedModelAgent()
		dm.backends["test/s"] = &managedBackend{be: plainFake{}}
		if got := dm.BackendLiveModel("test/s"); got != "" {
			t.Errorf("BackendLiveModel with a plain backend = %q, want \"\"", got)
		}
	})
}

// TestReportedSessionModel_UsesLiveBackendModel is the #2252 ticket scenario:
// an agent configured with the alias "opus" whose session's running backend
// already learned the exact id from its process, before the first turn
// completed. ReportedSessionModel must return the backend's id.
func TestReportedSessionModel_UsesLiveBackendModel(t *testing.T) {
	t.Parallel()

	a, dm := newReportedModelAgent()
	dm.backends["test/s"] = &managedBackend{be: &liveModelFake{running: true, model: "claude/claude-opus-5-5"}}

	if got := a.ReportedSessionModel("test/s"); got != "claude/claude-opus-5-5" {
		t.Errorf("ReportedSessionModel = %q, want the backend's live model %q", got, "claude/claude-opus-5-5")
	}
}

// TestReportedSessionModel_FallsBackToSessionModel pins every arm where no
// live model exists: ReportedSessionModel equals SessionModel (the agent
// default "opus") when there is no managed backend, the backend is not
// running, it reports no model, or it lacks the interface — and for an API
// agent with no DelegatedManager at all. Calling it never creates a managed
// backend.
func TestReportedSessionModel_FallsBackToSessionModel(t *testing.T) {
	t.Parallel()

	seeded := func(t *testing.T) (*Agent, *DelegatedManager) {
		t.Helper()
		a, dm := newReportedModelAgent()
		return a, dm
	}

	t.Run("no managed backend for the key", func(t *testing.T) {
		t.Parallel()
		a, dm := seeded(t)
		if got, want := a.ReportedSessionModel("test/absent"), a.SessionModel("test/absent"); got != want || want != "opus" {
			t.Errorf("ReportedSessionModel = %q, SessionModel = %q, want both %q", got, want, "opus")
		}
		if n := len(dm.backends); n != 0 {
			t.Errorf("backend count = %d after the call, want 0 (a read must not create a backend)", n)
		}
	})

	t.Run("backend not running", func(t *testing.T) {
		t.Parallel()
		a, dm := seeded(t)
		dm.backends["test/s"] = &managedBackend{be: &liveModelFake{running: false, model: "claude/claude-opus-5-5"}}
		if got := a.ReportedSessionModel("test/s"); got != "opus" {
			t.Errorf("ReportedSessionModel with a stopped backend = %q, want the alias %q", got, "opus")
		}
	})

	t.Run("backend reports no model", func(t *testing.T) {
		t.Parallel()
		a, dm := seeded(t)
		dm.backends["test/s"] = &managedBackend{be: &liveModelFake{running: true, model: ""}}
		if got := a.ReportedSessionModel("test/s"); got != "opus" {
			t.Errorf("ReportedSessionModel with an empty live model = %q, want the alias %q", got, "opus")
		}
	})

	t.Run("backend without the interface", func(t *testing.T) {
		t.Parallel()
		a, dm := seeded(t)
		dm.backends["test/s"] = &managedBackend{be: plainFake{}}
		if got := a.ReportedSessionModel("test/s"); got != "opus" {
			t.Errorf("ReportedSessionModel with a plain backend = %q, want the alias %q", got, "opus")
		}
	})

	t.Run("API agent", func(t *testing.T) {
		t.Parallel()
		a := &Agent{Model: "opus"}
		if got := a.ReportedSessionModel("test/s"); got != "opus" {
			t.Errorf("ReportedSessionModel for an API agent = %q, want SessionModel %q", got, "opus")
		}
	})
}
