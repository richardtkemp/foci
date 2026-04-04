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
