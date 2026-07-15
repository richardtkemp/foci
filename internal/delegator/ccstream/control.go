package ccstream

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"foci/internal/delegator"
)

// SendControl translates a backend-agnostic ControlRequest into the
// ccstream wire format and sends it to the CC subprocess.
func (b *Backend) SendControl(ctx context.Context, req delegator.ControlRequest) error {
	switch r := req.(type) {
	case *delegator.SetModelRequest:
		return b.sendSetModel(ctx, r.Model)
	case *delegator.SetPermissionModeRequest:
		b.mu.Lock()
		b.permMode = r.Mode
		b.mu.Unlock()
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

// sendSetModel sends a set_model control request and waits for CC's
// control_response, unlike set_permission_mode/apply_flag_settings which are
// intentionally fire-and-forget. CC validates the model id synchronously
// (e.g. rejects an unrecognized id) and reports success/failure in the
// response — without waiting for it, that signal was silently dropped
// (reqID discarded, OnControlResponse finds no waiter) and /model always
// reported "switched" even for a bogus model name. See controlResponseInbound
// for the verified wire shape.
func (b *Backend) sendSetModel(ctx context.Context, model string) error {
	reqID := newRequestID()

	ch := make(chan json.RawMessage, 1)
	b.pendingControlMu.Lock()
	if b.pendingControls == nil {
		b.pendingControls = make(map[string]chan json.RawMessage)
	}
	b.pendingControls[reqID] = ch
	b.pendingControlMu.Unlock()

	if err := b.writer.SendControl(reqID, &SetModelRequest{
		Subtype: "set_model",
		Model:   model,
	}); err != nil {
		b.pendingControlMu.Lock()
		delete(b.pendingControls, reqID)
		b.pendingControlMu.Unlock()
		return fmt.Errorf("send set_model: %w", err)
	}

	select {
	case raw := <-ch:
		var env controlResponseInbound
		if err := json.Unmarshal(raw, &env); err != nil {
			return fmt.Errorf("unmarshal set_model control_response: %w", err)
		}
		if env.Response.Subtype != "success" {
			if env.Response.Error != "" {
				return fmt.Errorf("%s", env.Response.Error)
			}
			return fmt.Errorf("set_model returned subtype %q", env.Response.Subtype)
		}
		return nil
	case <-ctx.Done():
		b.pendingControlMu.Lock()
		delete(b.pendingControls, reqID)
		b.pendingControlMu.Unlock()
		return ctx.Err()
	}
}

// Capabilities advertises ccstream's full mid-turn nudge support.
func (b *Backend) Capabilities() delegator.Capabilities {
	return delegator.CapabilitiesForBackend("claude-code")
}

// ccStreamCacheTTL is Claude Code's prompt-cache time-to-live. CC marks its
// prompt with 1-hour extended cache_control, so a session's cache stays warm
// for an hour after the last turn (not the Anthropic 5-minute default).
const ccStreamCacheTTL = time.Hour

// CacheTTL implements delegator.CacheTTLProvider: the prompt-cache warmth
// window the app uses to grey a cold session's avatar.
func (b *Backend) CacheTTL() time.Duration { return ccStreamCacheTTL }

// StatusDetail returns the current CC permission mode for /status display.
func (b *Backend) StatusDetail() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.permMode != "" {
		return "permission mode: " + b.permMode
	}
	return ""
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
		cats := make([]delegator.ContextCategory, len(payload.Categories))
		for i, c := range payload.Categories {
			cats[i] = delegator.ContextCategory{Name: c.Name, Tokens: c.Tokens}
		}
		return &delegator.ContextWindow{
			MaxTokens:   payload.MaxTokens,
			Model:       payload.Model,
			TotalTokens: payload.TotalTokens,
			Categories:  cats,
		}, nil
	case <-ctx.Done():
		// Clean up on timeout.
		b.pendingControlMu.Lock()
		delete(b.pendingControls, reqID)
		b.pendingControlMu.Unlock()
		return nil, ctx.Err()
	}
}
