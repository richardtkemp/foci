package cctmux

import (
	"testing"

	"foci/internal/delegator"
)

// TestBackend_Capabilities verifies cctmux declares its capabilities instead of
// falling through to the agent layer's default (#2154), and that it claims
// nothing it does not deliver: it never calls the nudge funcs and emits no
// text or thinking deltas.
func TestBackend_Capabilities(t *testing.T) {
	var _ delegator.BackendCapabilities = (*Backend)(nil)

	if got := (&Backend{}).Capabilities(); got != (delegator.Capabilities{}) {
		t.Errorf("Capabilities() = %+v, want none", got)
	}
}
