package codex

import (
	"context"
	"strings"
	"testing"

	"foci/internal/delegator"
	"foci/internal/log"
)

// newControlTestBackend returns a Backend minimally wired so that
// SendControl (which logs) and applyPendingControls run without a live
// app-server subprocess. Codex has no mid-session control channel —
// overrides queue up and are applied lazily at the next turn/start — so
// no transport is needed to exercise these paths.
func newControlTestBackend(t *testing.T) *Backend {
	t.Helper()
	return &Backend{
		lg:              log.NewComponentLogger("codex"),
		catalogueModels: []string{"gpt-5.2"},
	}
}

// ---------------------------------------------------------------------------
// SendControl — SetModelRequest
// ---------------------------------------------------------------------------

// TestSendControl_SetModel verifies that SetModelRequest stores the raw
// model name as pendingModel, and that applyPendingControls then injects
// it into turnStartParams.Model — the deferred-application contract
// (override takes effect on the next turn, since Codex has no mid-session
// model switch like CC's control_request protocol).
func TestSendControl_SetModel(t *testing.T) {
	t.Parallel()

	b := newControlTestBackend(t)

	if err := b.SendControl(context.Background(), &delegator.SetModelRequest{
		Model: "gpt-5.2",
	}); err != nil {
		t.Fatalf("SendControl: %v", err)
	}

	b.mu.Lock()
	model := b.pendingModel
	b.mu.Unlock()
	if model != "gpt-5.2" {
		t.Fatalf("pendingModel = %q, want %q", model, "gpt-5.2")
	}

	// The queued override must land in the next turn/start params.
	params := &turnStartParams{ThreadID: "th-1", Cwd: "/tmp"}
	b.applyPendingControls(params)
	if params.Model != "gpt-5.2" {
		t.Errorf("params.Model = %q, want %q", params.Model, "gpt-5.2")
	}
}

// ---------------------------------------------------------------------------
// SendControl — SetPermissionModeRequest
// ---------------------------------------------------------------------------

// TestSendControl_SetPermissionMode verifies that SetPermissionModeRequest
// translates foci permission-mode names into Codex approval policies
// (codexApprovalPolicy) and stores the result as pendingApproval. Codex
// applies it on the next turn/start via turnStartParams.ApprovalPolicy.
func TestSendControl_SetPermissionMode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		mode   string
		policy string
	}{
		{"default", "on-request"},
		{"acceptEdits", "on-request"},
		{"plan", "on-request"},
		{"bypassPermissions", "never"},
		{"dontAsk", "never"},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()

			b := newControlTestBackend(t)
			if err := b.SendControl(context.Background(), &delegator.SetPermissionModeRequest{
				Mode: tc.mode,
			}); err != nil {
				t.Fatalf("SendControl(%q): %v", tc.mode, err)
			}

			b.mu.Lock()
			got := b.pendingApproval
			b.mu.Unlock()
			if got != tc.policy {
				t.Errorf("pendingApproval = %q, want %q", got, tc.policy)
			}
		})
	}
}

// TestSendControl_SetPermissionMode_UnknownMode verifies that an
// unrecognized permission mode is rejected before any state is mutated.
func TestSendControl_SetPermissionMode_UnknownMode(t *testing.T) {
	t.Parallel()

	b := newControlTestBackend(t)
	err := b.SendControl(context.Background(), &delegator.SetPermissionModeRequest{
		Mode: "totally-bogus-mode",
	})
	if err == nil {
		t.Fatal("SendControl: got nil error, want rejection of unknown mode")
	}
	if !strings.Contains(err.Error(), "unknown permission mode") {
		t.Errorf("error = %q, want it to mention the unknown mode", err.Error())
	}

	b.mu.Lock()
	got := b.pendingApproval
	b.mu.Unlock()
	if got != "" {
		t.Errorf("pendingApproval = %q, want empty (no partial mutation)", got)
	}
}

// TestSendControl_Effort verifies a live /effort update is retained and sent
// through the app-server's turn/start effort field on the next turn.
func TestSendControl_Effort(t *testing.T) {
	t.Parallel()
	b := newControlTestBackend(t)
	if err := b.SendControl(context.Background(), &delegator.ApplyFlagSettingsRequest{
		Settings: map[string]any{"effortLevel": "ultra"},
	}); err != nil {
		t.Fatalf("SendControl: %v", err)
	}
	params := &turnStartParams{}
	b.applyPendingControls(params)
	if params.Effort != "ultra" {
		t.Errorf("Effort = %q, want ultra", params.Effort)
	}
}

// ---------------------------------------------------------------------------
// SendControl — unknown request type
// ---------------------------------------------------------------------------

// TestSendControl_UnknownRequestType verifies that an unrecognized
// ControlRequest falls through to the default branch and surfaces an
// error. The delegator.ControlRequest marker interface carries an
// unexported method, so no type outside package delegator can be
// constructed to satisfy it; a nil interface is the only way to reach
// the default branch from this test.
func TestSendControl_UnknownRequestType(t *testing.T) {
	t.Parallel()

	b := newControlTestBackend(t)
	err := b.SendControl(context.Background(), nil)
	if err == nil {
		t.Fatal("SendControl(nil): got nil error, want unsupported-type error")
	}
	if !strings.Contains(err.Error(), "unsupported control request type") {
		t.Errorf("error = %q, want unsupported-type message", err.Error())
	}
}

// ---------------------------------------------------------------------------
// applyPendingControls — merge behavior
// ---------------------------------------------------------------------------

// TestApplyPendingControls_MergesOverrides verifies that queued model and
// approval overrides are written into turnStartParams, overwriting any
// pre-existing values.
func TestApplyPendingControls_MergesOverrides(t *testing.T) {
	t.Parallel()

	b := newControlTestBackend(t)
	b.pendingModel = "o4-mini"
	b.pendingApproval = "never"

	params := &turnStartParams{
		ThreadID:       "th-merge",
		Model:          "stale-model",
		ApprovalPolicy: "on-request",
	}
	b.applyPendingControls(params)

	if params.Model != "o4-mini" {
		t.Errorf("Model = %q, want %q", params.Model, "o4-mini")
	}
	if params.ApprovalPolicy != "never" {
		t.Errorf("ApprovalPolicy = %q, want %q", params.ApprovalPolicy, "never")
	}
}

// TestApplyPendingControls_PreservesParamsWhenNoOverrides verifies that
// with nothing queued, applyPendingControls leaves existing params
// untouched (it must not clear an explicit Model/ApprovalPolicy set by
// beginTurn).
func TestApplyPendingControls_PreservesParamsWhenNoOverrides(t *testing.T) {
	t.Parallel()

	b := newControlTestBackend(t)

	params := &turnStartParams{
		ThreadID:       "th-keep",
		Model:          "preset-model",
		ApprovalPolicy: "on-request",
	}
	b.applyPendingControls(params)

	if params.Model != "preset-model" {
		t.Errorf("Model = %q, want %q (should be untouched)", params.Model, "preset-model")
	}
	if params.ApprovalPolicy != "on-request" {
		t.Errorf("ApprovalPolicy = %q, want %q (should be untouched)", params.ApprovalPolicy, "on-request")
	}
}

// TestApplyPendingControls_PartialMerge verifies that when only one
// override is queued, the other field is left alone.
func TestApplyPendingControls_PartialMerge(t *testing.T) {
	t.Parallel()

	// Only a model override: ApprovalPolicy must survive.
	b := newControlTestBackend(t)
	b.pendingModel = "only-model"

	params := &turnStartParams{ThreadID: "th-1", ApprovalPolicy: "on-request"}
	b.applyPendingControls(params)

	if params.Model != "only-model" {
		t.Errorf("Model = %q, want %q", params.Model, "only-model")
	}
	if params.ApprovalPolicy != "on-request" {
		t.Errorf("ApprovalPolicy = %q, want %q (untouched)", params.ApprovalPolicy, "on-request")
	}

	// Only an approval override: Model must survive.
	b2 := newControlTestBackend(t)
	b2.pendingApproval = "never"

	params2 := &turnStartParams{ThreadID: "th-2", Model: "preset-model"}
	b2.applyPendingControls(params2)

	if params2.Model != "preset-model" {
		t.Errorf("Model = %q, want %q (untouched)", params2.Model, "preset-model")
	}
	if params2.ApprovalPolicy != "never" {
		t.Errorf("ApprovalPolicy = %q, want %q", params2.ApprovalPolicy, "never")
	}
}
