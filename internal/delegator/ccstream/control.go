package ccstream

import (
	"context"
	"encoding/json"
	"fmt"

	"foci/internal/delegator"
)

// SendControl translates a backend-agnostic ControlRequest into the
// ccstream wire format and sends it to the CC subprocess.
func (b *Backend) SendControl(ctx context.Context, req delegator.ControlRequest) error {
	switch r := req.(type) {
	case *delegator.SetModelRequest:
		return b.writer.SendControl(newRequestID(), &SetModelRequest{
			Subtype: "set_model",
			Model:   r.Model,
		})
	case *delegator.SetPermissionModeRequest:
		return b.writer.SendControl(newRequestID(), &SetPermissionModeRequest{
			Subtype: "set_permission_mode",
			Mode:    r.Mode,
		})
	case *delegator.ApplyFlagSettingsRequest:
		return b.writer.SendControl(newRequestID(), &ApplyFlagSettingsRequest{
			Subtype:  "apply_flag_settings",
			Settings: r.Settings,
		})
	default:
		return fmt.Errorf("ccstream: unsupported control request type %T", req)
	}
}

// SendKeystroke is a no-op for the stream backend (no TUI).
func (b *Backend) SendKeystroke(ctx context.Context, key string) error {
	return fmt.Errorf("SendKeystroke not supported by stream backend")
}

// SendSpecialKey is a no-op for the stream backend (no TUI).
func (b *Backend) SendSpecialKey(ctx context.Context, key string) error {
	return fmt.Errorf("SendSpecialKey not supported by stream backend")
}

// Interrupt cancels the current agent turn by sending an interrupt control
// message over the stdio protocol.
func (b *Backend) Interrupt(ctx context.Context) error {
	return b.writer.SendInterrupt()
}

// SetModel sends a set_model control request to CC via the generic
// ControlSender interface. Convenience method retained for direct callers.
func (b *Backend) SetModel(ctx context.Context, model string) error {
	return b.SendControl(ctx, &delegator.SetModelRequest{Model: model})
}

// GetContextWindow sends a get_context_usage control request and returns the
// model's context window size. Zero API cost — CC computes this locally.
func (b *Backend) GetContextWindow(ctx context.Context) (*delegator.ContextWindow, error) {
	reqID := newRequestID()

	// Arm response channel before sending.
	ch := make(chan json.RawMessage, 1)
	b.pendingControlMu.Lock()
	if b.pendingControls == nil {
		b.pendingControls = make(map[string]chan json.RawMessage)
	}
	b.pendingControls[reqID] = ch
	b.pendingControlMu.Unlock()

	if err := b.writer.SendControl(reqID, &GetContextUsageRequest{
		Subtype: "get_context_usage",
	}); err != nil {
		// Clean up on send failure.
		b.pendingControlMu.Lock()
		delete(b.pendingControls, reqID)
		b.pendingControlMu.Unlock()
		return nil, fmt.Errorf("send get_context_usage: %w", err)
	}

	select {
	case raw := <-ch:
		var env controlResponseInbound
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, fmt.Errorf("unmarshal control_response envelope: %w", err)
		}
		if env.Response.Subtype != "success" {
			return nil, fmt.Errorf("get_context_usage returned subtype %q", env.Response.Subtype)
		}
		var payload contextUsagePayload
		if err := json.Unmarshal(env.Response.Response, &payload); err != nil {
			return nil, fmt.Errorf("unmarshal context_usage payload: %w", err)
		}
		return &delegator.ContextWindow{
			MaxTokens: payload.MaxTokens,
			Model:     payload.Model,
		}, nil
	case <-ctx.Done():
		// Clean up on timeout.
		b.pendingControlMu.Lock()
		delete(b.pendingControls, reqID)
		b.pendingControlMu.Unlock()
		return nil, ctx.Err()
	}
}
