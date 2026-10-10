package ccstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	"foci/internal/delegator"
)

// TestLiveModel_TracksInitAndAssistant proves the #2252 fix at the source:
// LiveModel reports the model CC's own process named — from system/init
// before any turn completes, then from each top-level assistant message —
// with the same prefixedModel spelling TurnResult.Model carries. No
// OnResult is fed: the value must be available mid-first-turn.
func TestLiveModel_TracksInitAndAssistant(t *testing.T) {
	t.Parallel()

	b := &Backend{
		readyCh:      make(chan struct{}),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  delegator.NewOutstandingRegistry(),
	}
	applyHandler(b, &testHandler{})

	feedInit(t, b, "claude-opus-5-5")
	if got := b.LiveModel(); got != "claude/claude-opus-5-5" {
		t.Errorf("LiveModel after init = %q, want %q (the resolved id, before any turn result)", got, "claude/claude-opus-5-5")
	}

	b.OnAssistant(&AssistantMessage{
		Message: BetaMessage{
			Model:   "claude-sonnet-4-6",
			Content: []ContentBlock{{Type: "text", Text: "working"}},
			Usage:   TokenUsage{InputTokens: 10, OutputTokens: 5},
		},
	})
	if got := b.LiveModel(); got != "claude/claude-sonnet-4-6" {
		t.Errorf("LiveModel after top-level assistant message = %q, want %q", got, "claude/claude-sonnet-4-6")
	}
}

// TestLiveModel_UnknownAndSyntheticStayEmpty pins LiveModel's empty arms: a
// backend whose process has not named a model yet reports "", and CC's
// synthetic sentinel (a no-API-call placeholder, never a launchable model)
// never leaks out as a reported model.
func TestLiveModel_UnknownAndSyntheticStayEmpty(t *testing.T) {
	t.Parallel()

	t.Run("no model learned yet", func(t *testing.T) {
		t.Parallel()
		b := &Backend{}
		applyHandler(b, &testHandler{})
		if got := b.LiveModel(); got != "" {
			t.Errorf("LiveModel on a fresh backend = %q, want \"\"", got)
		}
	})

	t.Run("synthetic assistant message keeps it empty", func(t *testing.T) {
		t.Parallel()
		b := &Backend{}
		applyHandler(b, &testHandler{})
		b.OnAssistant(&AssistantMessage{
			Message: BetaMessage{
				Model:   syntheticModel,
				Content: []ContentBlock{{Type: "text", Text: "No response requested."}},
			},
		})
		if got := b.LiveModel(); got != "" {
			t.Errorf("LiveModel after a synthetic message = %q, want \"\" (the sentinel is never reported)", got)
		}
	})
}

// newLiveModelControlBackend wires a Backend whose control plumbing works
// end-to-end (writer pipe + pendingControls): SendControl/set_model and
// GetContextWindow round-trip through OnControlResponse like they do against
// the real process.
func newLiveModelControlBackend(t *testing.T) (*Backend, func()) {
	t.Helper()
	pr, pw := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, pr) }() // drain so writer.SendControl doesn't block
	b := &Backend{
		writer:          NewWriter(pw),
		readyCh:         make(chan struct{}),
		pendingPerms:    make(map[string]*pendingPermission),
		pendingControls: make(map[string]chan json.RawMessage),
		outstanding:     delegator.NewOutstandingRegistry(),
	}
	applyHandler(b, &testHandler{})
	return b, func() { pw.Close() }
}

// feedInit names model via a system/init message, the process-start writer.
func feedInit(t *testing.T, b *Backend, model string) {
	t.Helper()
	raw := json.RawMessage(fmt.Sprintf(`{
		"type": "system",
		"subtype": "init",
		"claude_code_version": "2.1.293",
		"cwd": "/tmp",
		"model": %q,
		"permissionMode": "default",
		"tools": ["Bash"],
		"session_id": "sess-live-001"
	}`, model))
	b.OnSystem("init", raw)
}

// TestLiveModel_ConfirmedModelSwitchInvalidatesUntilRestated pins the /model
// interplay: a CONFIRMED set_model invalidates the learned id — the process
// switched, so the old id is stale (reporting it made whoami lag a /model
// switch while sessionMeta already held the new model) — and the
// get_context_usage response that /model's refresh issues re-learns the
// resolved id. No assistant message or result is fed: the invalidation and
// the re-learn are the two control-path writers.
func TestLiveModel_ConfirmedModelSwitchInvalidatesUntilRestated(t *testing.T) {
	t.Parallel()

	b, done := newLiveModelControlBackend(t)
	defer done()

	feedInit(t, b, "claude-opus-5-5")
	if got := b.LiveModel(); got != "claude/claude-opus-5-5" {
		t.Fatalf("LiveModel after init = %q, want %q", got, "claude/claude-opus-5-5")
	}

	// A confirmed switch to sonnet: CC accepts, so the learned opus id is stale.
	sendErr := make(chan error, 1)
	go func() {
		sendErr <- b.SendControl(context.Background(), &delegator.SetModelRequest{Model: "sonnet"})
	}()
	reqID := waitForPendingControl(t, b)
	b.OnControlResponse(json.RawMessage(fmt.Sprintf(`{"type":"control_response","response":{"subtype":"success","request_id":"%s"}}`, reqID)))
	if err := <-sendErr; err != nil {
		t.Fatalf("set_model: %v", err)
	}
	if got := b.LiveModel(); got != "" {
		t.Errorf("LiveModel after a confirmed set_model = %q, want \"\" (the pre-switch id is stale until the process restates one)", got)
	}

	// /model's refresh: get_context_usage names the resolved id.
	queryDone := make(chan error, 1)
	go func() {
		_, err := b.GetContextWindow(context.Background())
		queryDone <- err
	}()
	reqID = waitForPendingControl(t, b)
	b.OnControlResponse(json.RawMessage(fmt.Sprintf(`{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"totalTokens":1,"maxTokens":200000,"model":"claude-sonnet-4-6"}}}`, reqID)))
	if err := <-queryDone; err != nil {
		t.Fatalf("get_context_usage: %v", err)
	}
	if got := b.LiveModel(); got != "claude/claude-sonnet-4-6" {
		t.Errorf("LiveModel after get_context_usage named the model = %q, want %q", got, "claude/claude-sonnet-4-6")
	}
}

// TestLiveModel_RejectedModelSwitchKeepsLearnedModel pins the other arm of
// the set_model writer: CC rejecting the switch (e.g. an unrecognized id)
// means the process still runs the learned model, so LiveModel keeps
// reporting it — only a CONFIRMED switch invalidates.
func TestLiveModel_RejectedModelSwitchKeepsLearnedModel(t *testing.T) {
	t.Parallel()

	b, done := newLiveModelControlBackend(t)
	defer done()

	feedInit(t, b, "claude-opus-5-5")
	if got := b.LiveModel(); got != "claude/claude-opus-5-5" {
		t.Fatalf("LiveModel after init = %q, want %q", got, "claude/claude-opus-5-5")
	}

	sendErr := make(chan error, 1)
	go func() {
		sendErr <- b.SendControl(context.Background(), &delegator.SetModelRequest{Model: "this-model-does-not-exist-xyz"})
	}()
	reqID := waitForPendingControl(t, b)
	b.OnControlResponse(json.RawMessage(fmt.Sprintf(`{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"not a recognized model id"}}`, reqID)))
	if err := <-sendErr; err == nil {
		t.Fatal("set_model: got nil error, want the rejection surfaced")
	}
	if got := b.LiveModel(); got != "claude/claude-opus-5-5" {
		t.Errorf("LiveModel after a rejected set_model = %q, want the still-running %q", got, "claude/claude-opus-5-5")
	}
}
