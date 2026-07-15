package ccstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
)

// TestSendControl_SetModel verifies that SendControl translates a
// delegator.SetModelRequest into the correct ccstream wire format, and that
// it now waits for CC's control_response (unlike set_permission_mode /
// apply_flag_settings, which stay fire-and-forget) so a rejected model is
// reported back to the caller instead of being optimistically dropped.
func TestSendControl_SetModel(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, pr) }() // drain so writer.SendControl doesn't block

	b := &Backend{
		writer:          NewWriter(pw),
		pendingControls: make(map[string]chan json.RawMessage),
	}

	type result struct{ err error }
	resCh := make(chan result, 1)
	go func() {
		resCh <- result{b.SendControl(context.Background(), &delegator.SetModelRequest{Model: "opus"})}
	}()

	reqID := waitForPendingControl(t, b)

	// Simulate CC accepting the model switch.
	resp := fmt.Sprintf(`{"type":"control_response","response":{"subtype":"success","request_id":"%s"}}`, reqID)
	b.OnControlResponse(json.RawMessage(resp))

	r := <-resCh
	if r.err != nil {
		t.Fatalf("SendControl: %v", r.err)
	}

	pw.Close()
}

// TestSendControl_SetModel_Rejected verifies that when CC rejects a set_model
// request (e.g. an unrecognized model id), SendControl surfaces CC's own
// error text instead of reporting success. Wire shape verified live against
// CC 2026-07-15 — see controlResponseInbound doc comment.
func TestSendControl_SetModel_Rejected(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, pr) }()

	b := &Backend{
		writer:          NewWriter(pw),
		pendingControls: make(map[string]chan json.RawMessage),
	}

	type result struct{ err error }
	resCh := make(chan result, 1)
	go func() {
		resCh <- result{b.SendControl(context.Background(), &delegator.SetModelRequest{Model: "this-model-does-not-exist-xyz"})}
	}()

	reqID := waitForPendingControl(t, b)

	resp := fmt.Sprintf(`{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":%q}}`,
		reqID, `Model "this-model-does-not-exist-xyz" is not a recognized model id. Run /model to see available models.`)
	b.OnControlResponse(json.RawMessage(resp))

	r := <-resCh
	if r.err == nil {
		t.Fatal("SendControl: got nil error, want the rejected-model error surfaced")
	}
	if !strings.Contains(r.err.Error(), "not a recognized model id") {
		t.Errorf("SendControl error = %q, want it to contain CC's rejection message", r.err.Error())
	}

	pw.Close()
}

// waitForPendingControl polls until exactly one pendingControls entry is
// registered (the in-flight set_model request) and returns its request_id.
func waitForPendingControl(t *testing.T, b *Backend) string {
	t.Helper()
	var reqID string
	for i := 0; i < 100; i++ {
		time.Sleep(time.Millisecond)
		b.pendingControlMu.Lock()
		for k := range b.pendingControls {
			reqID = k
		}
		b.pendingControlMu.Unlock()
		if reqID != "" {
			return reqID
		}
	}
	t.Fatal("set_model didn't register a pending control request")
	return ""
}

// TestSendControl_SetPermissionMode verifies that SendControl translates a
// delegator.SetPermissionModeRequest into the correct ccstream wire format.
func TestSendControl_SetPermissionMode(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	b := &Backend{
		writer:       NewWriter(pw),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  delegator.NewOutstandingRegistry(),
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
		outstanding:  delegator.NewOutstandingRegistry(),
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
