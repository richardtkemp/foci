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
