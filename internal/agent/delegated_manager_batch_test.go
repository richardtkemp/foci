package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"foci/internal/delegator"
)

// mockBatchBackend is a Delegator that also implements delegator.BatchRunner.
type mockBatchBackend struct {
	mockBackendDM
	mu     sync.Mutex
	gotReq delegator.BatchRequest
	resp   string
}

func (m *mockBatchBackend) RunBatch(_ context.Context, req delegator.BatchRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gotReq = req
	return m.resp, nil
}

func TestRunOnce_DispatchesToBatchRunner(t *testing.T) {
	// A backend implementing BatchRunner gets the one-shot; the legacy
	// claude --print path must NOT run (ClaudeBinary points nowhere, so
	// falling through would error).
	t.Parallel()

	mb := &mockBatchBackend{resp: "batch result"}
	m := &DelegatedManager{
		AgentID: "test",
		StartOpts: delegator.StartOptions{
			WorkDir:      "/tmp/test-workdir",
			AgentID:      "test",
			ClaudeBinary: "/nonexistent/claude",
		},
		NewBackend: func() (delegator.Delegator, error) { return mb, nil },
	}

	got, err := m.RunOnce(context.Background(), "the prompt", "the system prompt")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got != "batch result" {
		t.Errorf("result = %q", got)
	}
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if mb.gotReq.Prompt != "the prompt" || mb.gotReq.SystemPrompt != "the system prompt" {
		t.Errorf("request = %+v", mb.gotReq)
	}
	if mb.gotReq.WorkDir != "/tmp/test-workdir" || mb.gotReq.AgentID != "test" {
		t.Errorf("workdir/agent not threaded: %+v", mb.gotReq)
	}
	if mb.gotReq.Model != "" {
		t.Errorf("Model should be empty (backend picks its batch default), got %q", mb.gotReq.Model)
	}
}

func TestRunOnceWithModel_ThreadsModel(t *testing.T) {
	// #1309: RunOnceWithModel is RunOnce plus an explicit model override,
	// implementing nudge.ModelOneShotRunner so extraction (or any other
	// one-shot caller) can use a model other than the backend's hardcoded
	// default.
	t.Parallel()

	mb := &mockBatchBackend{resp: "batch result"}
	m := &DelegatedManager{
		AgentID: "test",
		StartOpts: delegator.StartOptions{
			WorkDir:      "/tmp/test-workdir",
			AgentID:      "test",
			ClaudeBinary: "/nonexistent/claude",
		},
		NewBackend: func() (delegator.Delegator, error) { return mb, nil },
	}

	got, err := m.RunOnceWithModel(context.Background(), "the prompt", "the system prompt", "haiku")
	if err != nil {
		t.Fatalf("RunOnceWithModel: %v", err)
	}
	if got != "batch result" {
		t.Errorf("result = %q", got)
	}
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if mb.gotReq.Model != "haiku" {
		t.Errorf("Model = %q, want haiku", mb.gotReq.Model)
	}
	if mb.gotReq.Prompt != "the prompt" || mb.gotReq.SystemPrompt != "the system prompt" {
		t.Errorf("request = %+v", mb.gotReq)
	}
	if mb.gotReq.WorkDir != "/tmp/test-workdir" || mb.gotReq.AgentID != "test" {
		t.Errorf("workdir/agent not threaded: %+v", mb.gotReq)
	}
}

func TestRunBatch_ThreadsModelAndDefaultsWorkDirAgentID(t *testing.T) {
	// RunBatch is the general entry point behind RunOnce: it must forward a
	// caller-supplied Model untouched (RunOnce never sets one), and fill in
	// WorkDir/AgentID from the manager's own StartOpts when the caller leaves
	// them empty — the shape tools.BatchSummariser (#1317) relies on so it can
	// pass Model="haiku" without also having to know the agent's workdir/ID.
	t.Parallel()

	mb := &mockBatchBackend{resp: "batch result"}
	m := &DelegatedManager{
		AgentID: "test",
		StartOpts: delegator.StartOptions{
			WorkDir: "/tmp/test-workdir",
			AgentID: "test-agent",
		},
		NewBackend: func() (delegator.Delegator, error) { return mb, nil },
	}

	got, err := m.RunBatch(context.Background(), delegator.BatchRequest{
		Prompt:       "the prompt",
		SystemPrompt: "the system prompt",
		Model:        "haiku",
	})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	if got != "batch result" {
		t.Errorf("result = %q", got)
	}

	mb.mu.Lock()
	defer mb.mu.Unlock()
	if mb.gotReq.Model != "haiku" {
		t.Errorf("Model = %q, want haiku", mb.gotReq.Model)
	}
	if mb.gotReq.WorkDir != "/tmp/test-workdir" || mb.gotReq.AgentID != "test-agent" {
		t.Errorf("WorkDir/AgentID not defaulted from StartOpts: %+v", mb.gotReq)
	}
}

func TestRunBatch_CallerWorkDirAgentIDWin(t *testing.T) {
	// A caller-supplied WorkDir/AgentID must NOT be overwritten by the
	// manager's StartOpts defaults.
	t.Parallel()

	mb := &mockBatchBackend{resp: "ok"}
	m := &DelegatedManager{
		StartOpts: delegator.StartOptions{
			WorkDir: "/default/workdir",
			AgentID: "default-agent",
		},
		NewBackend: func() (delegator.Delegator, error) { return mb, nil },
	}

	_, err := m.RunBatch(context.Background(), delegator.BatchRequest{
		Prompt:  "p",
		WorkDir: "/caller/workdir",
		AgentID: "caller-agent",
	})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}

	mb.mu.Lock()
	defer mb.mu.Unlock()
	if mb.gotReq.WorkDir != "/caller/workdir" || mb.gotReq.AgentID != "caller-agent" {
		t.Errorf("caller-supplied WorkDir/AgentID overwritten: %+v", mb.gotReq)
	}
}

func TestRunOnce_ErrorsWithoutBatchRunner(t *testing.T) {
	// A backend without BatchRunner (cctmux) gets a clear error — NOT the old
	// silent claude --print fallback, which ran one-shots on a different
	// vendor's CLI than the agent's backend.
	t.Parallel()

	m := &DelegatedManager{
		AgentID: "test",
		StartOpts: delegator.StartOptions{
			WorkDir:      t.TempDir(),
			AgentID:      "test",
			ClaudeBinary: "/nonexistent/claude", // must never be invoked
		},
		NewBackend: func() (delegator.Delegator, error) { return &mockBackendDM{}, nil },
	}

	_, err := m.RunOnce(context.Background(), "p", "s")
	if err == nil || !strings.Contains(err.Error(), "does not implement delegator.BatchRunner") {
		t.Errorf("expected BatchRunner hint error, got %v", err)
	}
}
