package agent

import (
	"context"
	"os"
	"path/filepath"
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

func TestRunOnce_LegacyFallbackWithoutBatchRunner(t *testing.T) {
	// A backend without BatchRunner (cctmux) falls back to the historical
	// direct `claude --print`, honouring StartOpts.ClaudeBinary.
	t.Parallel()

	dir := t.TempDir()
	stub := filepath.Join(dir, "claude")
	script := "#!/bin/sh\necho \"ARGS:$*\" > " + filepath.Join(dir, "capture.txt") + "\nprintf 'legacy result'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	m := &DelegatedManager{
		AgentID: "test",
		StartOpts: delegator.StartOptions{
			WorkDir:      dir,
			AgentID:      "test",
			ClaudeBinary: stub,
		},
		NewBackend: func() (delegator.Delegator, error) { return &mockBackendDM{}, nil },
	}

	got, err := m.RunOnce(context.Background(), "p", "s")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got != "legacy result" {
		t.Errorf("result = %q", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "capture.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "--system-prompt s") {
		t.Errorf("legacy path lost system prompt: %s", data)
	}
}
