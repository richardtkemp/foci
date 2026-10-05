package delegator

import "testing"

// TestCapabilitiesForBackend pins each backend's declared capabilities to what
// it implements (#2154). cctmux used to share claude-code's row and claimed
// mid-turn nudges and streaming, none of which it delivers.
func TestCapabilitiesForBackend(t *testing.T) {
	tests := []struct {
		backend string
		want    Capabilities
	}{
		{"claude-code", Capabilities{PostToolNudge: true, PreAnswerNudge: true, Streaming: true}},
		{"claude-code-tmux", Capabilities{}},
		{"opencode", Capabilities{Streaming: true}},
		{"codex", Capabilities{Streaming: true}},
		{"some-future-backend", Capabilities{}},
	}
	for _, tt := range tests {
		if got := CapabilitiesForBackend(tt.backend); got != tt.want {
			t.Errorf("CapabilitiesForBackend(%q) = %+v, want %+v", tt.backend, got, tt.want)
		}
	}
}
