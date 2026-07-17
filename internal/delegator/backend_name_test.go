package delegator

import "testing"

// TestHumanReadableBackendName verifies known backend types map to their
// display names, and unknown/empty types degrade sensibly rather than
// returning a blank string.
func TestHumanReadableBackendName(t *testing.T) {
	tests := []struct {
		backendType string
		want        string
	}{
		{"claude-code", "Claude Code"},
		{"claude-code-tmux", "Claude Code"},
		{"codex", "Codex CLI"},
		{"opencode", "OpenCode"},
		{"", "the delegated backend"},
		{"some-future-backend", "some-future-backend"},
	}
	for _, tt := range tests {
		if got := HumanReadableBackendName(tt.backendType); got != tt.want {
			t.Errorf("HumanReadableBackendName(%q) = %q, want %q", tt.backendType, got, tt.want)
		}
	}
}
