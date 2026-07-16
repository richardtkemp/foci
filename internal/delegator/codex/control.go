package codex

import (
	"context"
	"fmt"

	"foci/internal/delegator"
)

// SendControl translates a backend-agnostic ControlRequest into Codex
// state changes. Codex doesn't have mid-session control requests like CC's
// control_request protocol — model/effort/approval changes take effect on
// the next turn/start. SendControl stores the override; beginTurn applies it.
func (b *Backend) SendControl(ctx context.Context, req delegator.ControlRequest) error {
	switch r := req.(type) {
	case *delegator.SetModelRequest:
		resolution, err := b.ResolveModel(ctx, r.Model)
		if err != nil {
			return err
		}
		b.mu.Lock()
		b.pendingModel = resolution.BackendModel
		b.mu.Unlock()
		b.lg.Infof("model override queued: %s (applies next turn)", resolution.BackendModel)
		return nil

	case *delegator.SetPermissionModeRequest:
		mode := codexApprovalPolicy(r.Mode)
		if mode == "" {
			return fmt.Errorf("codex: unknown permission mode %q", r.Mode)
		}
		b.mu.Lock()
		b.pendingApproval = mode
		b.mu.Unlock()
		b.lg.Infof("approval policy override queued: %s (applies next turn)", mode)
		return nil

	case *delegator.ApplyFlagSettingsRequest:
		if effort, ok := r.Settings["effortLevel"].(string); ok {
			b.mu.Lock()
			b.pendingEffort = effort
			b.mu.Unlock()
			b.lg.Infof("effort override queued: %s (applies next turn)", effort)
		}
		return nil

	default:
		return fmt.Errorf("codex: unsupported control request type %T", req)
	}
}

// codexApprovalPolicy translates foci permission mode names to Codex
// approval policy values for turn/start.
func codexApprovalPolicy(mode string) string {
	switch mode {
	case "default", "acceptEdits":
		return "on-request"
	case "bypassPermissions", "dontAsk":
		return "never"
	case "plan":
		return "on-request"
	default:
		return ""
	}
}

// applyPendingControls merges queued overrides into turn/start params.
// Called from beginTurn before sending the request.
func (b *Backend) applyPendingControls(params *turnStartParams) {
	b.mu.Lock()
	model := b.pendingModel
	effort := b.pendingEffort
	approval := b.pendingApproval
	b.mu.Unlock()

	if model != "" {
		params.Model = model
	}
	if effort != "" && effort != "off" {
		params.Effort = effort
	}
	if approval != "" {
		params.ApprovalPolicy = approval
	}
}
