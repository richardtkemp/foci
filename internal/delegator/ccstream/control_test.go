package ccstream

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"foci/internal/delegator"
)

// TestSendControl_SetModel verifies that SendControl translates a
// delegator.SetModelRequest into the correct ccstream wire format.
func TestSendControl_SetModel(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	b := &Backend{
		writer:       NewWriter(pw),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  NewOutstandingRegistry(),
	}

	done := make(chan struct{})
	var line []byte
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		n, _ := pr.Read(buf)
		line = buf[:n]
	}()

	err := b.SendControl(context.Background(), &delegator.SetModelRequest{Model: "opus"})
	if err != nil {
		t.Fatalf("SendControl: %v", err)
	}

	pw.Close()
	<-done

	// Parse the wire message.
	var env struct {
		Type      string          `json:"type"`
		RequestID string          `json:"request_id"`
		Request   json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Type != "control_request" {
		t.Errorf("type = %q, want %q", env.Type, "control_request")
	}
	if env.RequestID == "" {
		t.Error("request_id is empty")
	}

	var req struct {
		Subtype string `json:"subtype"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(env.Request, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if req.Subtype != "set_model" {
		t.Errorf("subtype = %q, want %q", req.Subtype, "set_model")
	}
	if req.Model != "opus" {
		t.Errorf("model = %q, want %q", req.Model, "opus")
	}
}

// TestSendControl_SetPermissionMode verifies that SendControl translates a
// delegator.SetPermissionModeRequest into the correct ccstream wire format.
func TestSendControl_SetPermissionMode(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	b := &Backend{
		writer:       NewWriter(pw),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  NewOutstandingRegistry(),
	}

	done := make(chan struct{})
	var line []byte
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		n, _ := pr.Read(buf)
		line = buf[:n]
	}()

	err := b.SendControl(context.Background(), &delegator.SetPermissionModeRequest{Mode: "plan"})
	if err != nil {
		t.Fatalf("SendControl: %v", err)
	}

	pw.Close()
	<-done

	var env struct {
		Type      string          `json:"type"`
		RequestID string          `json:"request_id"`
		Request   json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Type != "control_request" {
		t.Errorf("type = %q, want %q", env.Type, "control_request")
	}
	if env.RequestID == "" {
		t.Error("request_id is empty")
	}

	var req struct {
		Subtype string `json:"subtype"`
		Mode    string `json:"mode"`
	}
	if err := json.Unmarshal(env.Request, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if req.Subtype != "set_permission_mode" {
		t.Errorf("subtype = %q, want %q", req.Subtype, "set_permission_mode")
	}
	if req.Mode != "plan" {
		t.Errorf("mode = %q, want %q", req.Mode, "plan")
	}
}

// TestSendControl_ApplyFlagSettings verifies that SendControl translates a
// delegator.ApplyFlagSettingsRequest into the apply_flag_settings wire format,
// carrying the settings record (e.g. effortLevel) verbatim.
func TestSendControl_ApplyFlagSettings(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	b := &Backend{
		writer:       NewWriter(pw),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  NewOutstandingRegistry(),
	}

	done := make(chan struct{})
	var line []byte
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		n, _ := pr.Read(buf)
		line = buf[:n]
	}()

	err := b.SendControl(context.Background(), &delegator.ApplyFlagSettingsRequest{
		Settings: map[string]any{"effortLevel": "max"},
	})
	if err != nil {
		t.Fatalf("SendControl: %v", err)
	}

	pw.Close()
	<-done

	var env struct {
		Type    string          `json:"type"`
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Type != "control_request" {
		t.Errorf("type = %q, want %q", env.Type, "control_request")
	}

	var req struct {
		Subtype  string         `json:"subtype"`
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(env.Request, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if req.Subtype != "apply_flag_settings" {
		t.Errorf("subtype = %q, want %q", req.Subtype, "apply_flag_settings")
	}
	if req.Settings["effortLevel"] != "max" {
		t.Errorf("settings.effortLevel = %v, want %q", req.Settings["effortLevel"], "max")
	}
}

// Note: Testing unsupported ControlRequest types is not possible from
// outside the backend package — the marker interface's unexported method
// prevents external types from satisfying it. This is by design.
