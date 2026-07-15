// control.go — delegator.ControlSender + Interrupt implementation.
// Translates foci's backend-agnostic control intents into opencode's
// HTTP API.

package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"foci/internal/delegator"
	"foci/internal/log"
)

// SendControl dispatches a ControlRequest to opencode's HTTP API.
// Implements delegator.ControlSender.
//
//	SetModelRequest          → store b.model (applied per-prompt via body "model" field)
//	SetPermissionModeRequest → PATCH /config {permission: {…}}
//	ApplyFlagSettingsRequest → no-op (effort has no opencode equivalent)
//	other                    → error
func (b *Backend) SendControl(ctx context.Context, req delegator.ControlRequest) error {
	switch r := req.(type) {
	case *delegator.SetModelRequest:
		binaryPath, _ := b.cfg["binary"].(string)
		resolved, err := b.resolveModelFn(ctx, binaryPath, b.workDir, r.Model)
		if err != nil {
			return fmt.Errorf("opencode: %w", err)
		}
		b.mu.Lock()
		b.model = resolved
		b.mu.Unlock()
		return nil

	case *delegator.SetPermissionModeRequest:
		return b.patchConfig(ctx, map[string]any{
			"permission": mapPermissionMode(r.Mode),
		})

	case *delegator.ApplyFlagSettingsRequest:
		// opencode has no apply_flag_settings equivalent. Effort is
		// CC-only. Fire-and-forget — return nil.
		log.NewComponentLogger(b.logComponent()).Debugf("SendControl: ApplyFlagSettings is a no-op for opencode")
		return nil

	default:
		return fmt.Errorf("opencode: unsupported control request %T", req)
	}
}

// Interrupt aborts any in-flight turn via POST /session/:id/abort.
// Implements delegator.Delegator.Interrupt.
func (b *Backend) Interrupt(ctx context.Context) error {
	if b.server == nil || b.sessionID == "" {
		return errors.New("opencode: Interrupt before Start")
	}
	url := fmt.Sprintf("%s/session/%s/abort", b.server.baseURL, b.sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("POST /abort: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("POST /abort: HTTP %d", resp.StatusCode)
	}
	return nil
}

// patchConfig sends a PATCH /config to the opencode server. The body is
// a partial Config object — opencode merges it with the existing config.
func (b *Backend) patchConfig(ctx context.Context, body map[string]any) error {
	if b.server == nil || b.server.baseURL == "" {
		return errors.New("opencode: no server (Start not called)")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := b.server.baseURL + "/config"
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("PATCH /config: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("PATCH /config: HTTP %d", resp.StatusCode)
	}
	return nil
}

// mapPermissionMode translates foci's permission modes to opencode's
// per-category ask|allow|deny rules. opencode's permission config uses
// wildcards + per-tool overrides.
//
// For v1 we apply a coarse mapping: "bypassPermissions" → allow all;
// "acceptEdits" → allow edits + ask for bash; "plan" → deny edits;
// default → ask for everything.
func mapPermissionMode(mode string) map[string]any {
	switch mode {
	case "bypassPermissions", "auto":
		return map[string]any{"*": "allow"}
	case "acceptEdits":
		return map[string]any{
			"edit": "allow",
			"bash": "ask",
			"*":    "ask",
		}
	case "plan":
		return map[string]any{
			"edit": "deny",
			"read": "allow",
			"*":    "ask",
		}
	default:
		return map[string]any{
			"edit": "ask",
			"bash": "ask",
			"*":    "ask",
		}
	}
}
