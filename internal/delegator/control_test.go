package delegator

import "testing"

// TestSetModelRequestImplementsControlRequest verifies compile-time interface
// satisfaction for the marker interface.
func TestSetModelRequestImplementsControlRequest(t *testing.T) {
	var _ ControlRequest = (*SetModelRequest)(nil)
}

// TestSetModelRequestFields verifies the intent type carries the model name.
func TestSetModelRequestFields(t *testing.T) {
	req := &SetModelRequest{Model: "opus"}
	if req.Model != "opus" {
		t.Errorf("Model = %q, want %q", req.Model, "opus")
	}
}

// TestSetPermissionModeRequestImplementsControlRequest verifies compile-time
// interface satisfaction for the marker interface.
func TestSetPermissionModeRequestImplementsControlRequest(t *testing.T) {
	var _ ControlRequest = (*SetPermissionModeRequest)(nil)
}

// TestSetPermissionModeRequestFields verifies the intent type carries the mode.
func TestSetPermissionModeRequestFields(t *testing.T) {
	req := &SetPermissionModeRequest{Mode: "plan"}
	if req.Mode != "plan" {
		t.Errorf("Mode = %q, want %q", req.Mode, "plan")
	}
}
