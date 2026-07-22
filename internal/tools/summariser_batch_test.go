package tools

import (
	"context"
	"strings"
	"testing"

	"foci/internal/delegator"
)

// mockBatchRunner is a tools.BatchRunner test double that records the request
// it received and returns a canned response (or error).
type mockBatchRunner struct {
	gotReq delegator.BatchRequest
	resp   string
	err    error
}

func (m *mockBatchRunner) RunBatch(_ context.Context, req delegator.BatchRequest) (string, error) {
	m.gotReq = req
	return m.resp, m.err
}

func TestBatchSummariser_DispatchesViaRunner(t *testing.T) {
	// Proves BatchSummariser routes the summarise call through the resolved
	// BatchRunner — with the model preference (haiku) threaded onto the
	// request — rather than shelling `claude --print` directly (the
	// CLISummariser behaviour foci_todo #1317 replaces).
	t.Parallel()

	runner := &mockBatchRunner{resp: "  the summary  "}
	s := NewBatchSummariser(func() BatchRunner { return runner }, "haiku", "/workdir", "agent-1", func() int { return 0 })

	got, err := s.Summarise(context.Background(), []byte("file content"), "what does this do?", "foo.go")
	if err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	if got != "the summary" {
		t.Errorf("result = %q, want trimmed %q", got, "the summary")
	}

	if runner.gotReq.Model != "haiku" {
		t.Errorf("Model = %q, want haiku", runner.gotReq.Model)
	}
	if runner.gotReq.WorkDir != "/workdir" || runner.gotReq.AgentID != "agent-1" {
		t.Errorf("workdir/agent not threaded: %+v", runner.gotReq)
	}
	if runner.gotReq.SystemPrompt != summarySystemPrompt {
		t.Errorf("SystemPrompt = %q, want the shared summarySystemPrompt", runner.gotReq.SystemPrompt)
	}
	if !strings.Contains(runner.gotReq.Prompt, "file content") || !strings.Contains(runner.gotReq.Prompt, "what does this do?") {
		t.Errorf("Prompt missing content/prompt envelope: %q", runner.gotReq.Prompt)
	}
	if !strings.Contains(runner.gotReq.Prompt, `path="foo.go"`) {
		t.Errorf("Prompt missing file path wrapper: %q", runner.gotReq.Prompt)
	}
}

func TestBatchSummariser_EmptyResponse(t *testing.T) {
	// Proves an empty (or whitespace-only) model response surfaces as the
	// documented "(empty response)" sentinel, matching APISummariser/the old
	// CLISummariser behaviour.
	t.Parallel()

	runner := &mockBatchRunner{resp: "   "}
	s := NewBatchSummariser(func() BatchRunner { return runner }, "haiku", "", "", func() int { return 0 })

	got, err := s.Summarise(context.Background(), []byte("x"), "prompt", "")
	if err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	if got != "(empty response)" {
		t.Errorf("result = %q, want sentinel", got)
	}
}

func TestBatchSummariser_RunnerError(t *testing.T) {
	// Proves a RunBatch failure propagates rather than being swallowed.
	t.Parallel()

	runner := &mockBatchRunner{err: errBoom}
	s := NewBatchSummariser(func() BatchRunner { return runner }, "haiku", "", "", func() int { return 0 })

	_, err := s.Summarise(context.Background(), []byte("x"), "prompt", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestBatchSummariser_NilRunner(t *testing.T) {
	// Proves that when the runner resolves to nil — the agent's
	// DelegatedManager isn't configured yet, or isn't a delegated agent at
	// all — Summarise reports a clear error instead of panicking on a nil
	// dereference. This is the state buildExecRegistry's summariser sits in
	// between construction and configureDelegated assigning
	// ag.DelegatedManager (see agents_delegated.go).
	t.Parallel()

	s := NewBatchSummariser(func() BatchRunner { return nil }, "haiku", "", "", func() int { return 0 })

	_, err := s.Summarise(context.Background(), []byte("x"), "prompt", "")
	if err == nil || !strings.Contains(err.Error(), "no BatchRunner available") {
		t.Errorf("expected 'no BatchRunner available' error, got %v", err)
	}
}

func TestBatchSummariser_CapsInput(t *testing.T) {
	// Proves BatchSummariser applies the same CapInputChars truncation as the
	// other Summariser implementations, before building the request envelope.
	t.Parallel()

	runner := &mockBatchRunner{resp: "ok"}
	s := NewBatchSummariser(func() BatchRunner { return runner }, "haiku", "", "", func() int { return 5 })

	_, err := s.Summarise(context.Background(), []byte("0123456789"), "prompt", "")
	if err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	if !strings.Contains(runner.gotReq.Prompt, "truncated") {
		t.Errorf("expected truncation annotation in prompt, got %q", runner.gotReq.Prompt)
	}
	if strings.Contains(runner.gotReq.Prompt, "56789") {
		t.Errorf("expected content truncated before char 5, got %q", runner.gotReq.Prompt)
	}
}

var errBoom = &boomError{}

type boomError struct{}

func (*boomError) Error() string { return "boom" }
