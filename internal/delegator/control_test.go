package delegator

import (
	"context"
	"testing"
)

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

type testModelResolver struct{}

func (testModelResolver) ResolveModel(context.Context, string) (ModelResolution, error) {
	return ModelResolution{}, nil
}

// TestModelResolverInterface proves catalogue-backed delegators can expose
// canonical model resolution without changing the ControlSender contract.
func TestModelResolverInterface(t *testing.T) {
	var _ ModelResolver = testModelResolver{}
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
