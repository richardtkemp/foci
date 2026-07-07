package opencode

import (
	"testing"

	"foci/internal/delegator"
)

func TestBackend_Capabilities(t *testing.T) {
	b := &Backend{}
	caps := b.Capabilities()

	if caps.PostToolNudge {
		t.Error("opencode should not advertise PostToolNudge (HTTP/SSE, no stdin pipe)")
	}
	if caps.PreAnswerNudge {
		t.Error("opencode should not advertise PreAnswerNudge (HTTP/SSE, no stdin pipe)")
	}
}

func TestBackend_ImplementsBackendCapabilities(t *testing.T) {
	var _ delegator.BackendCapabilities = (*Backend)(nil)
}
