package ccstream

import (
	"testing"

	"foci/internal/delegator"
)

func TestBackend_Capabilities(t *testing.T) {
	b := &Backend{}
	caps := b.Capabilities()

	if !caps.PostToolNudge {
		t.Error("ccstream should advertise PostToolNudge (stdin pipe → mid-turn injection)")
	}
	if !caps.PreAnswerNudge {
		t.Error("ccstream should advertise PreAnswerNudge (stdin pipe → mid-turn injection)")
	}
}

func TestBackend_ImplementsBackendCapabilities(t *testing.T) {
	var _ delegator.BackendCapabilities = (*Backend)(nil)
}
