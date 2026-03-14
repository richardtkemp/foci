package provision

import (
	"testing"
)

func TestIsValidAgentID(t *testing.T) {
	// Verifies agent ID validation rules: lowercase letters, numbers, hyphens only.
	valid := []string{"main", "fotini", "my-agent", "agent1", "a", "x123-test"}
	for _, s := range valid {
		if !IsValidAgentID(s) {
			t.Errorf("IsValidAgentID(%q) = false, want true", s)
		}
	}

	invalid := []string{"", "Main", "-start", "1start", "has space", "has_under", "has.dot"}
	for _, s := range invalid {
		if IsValidAgentID(s) {
			t.Errorf("IsValidAgentID(%q) = true, want false", s)
		}
	}
}

func TestResolveModelAlias(t *testing.T) {
	// Verifies model alias resolution for opus, sonnet, haiku shorthands.
	tests := []struct {
		input string
		want  string
	}{
		{"opus", "anthropic/claude-opus-4-6"},
		{"sonnet", "anthropic/claude-sonnet-4-6"},
		{"haiku", "anthropic/claude-haiku-4-5-20251001"},
		{"", "anthropic/claude-sonnet-4-6"},
		{"claude-custom-model", "claude-custom-model"},
		{"OPUS", "anthropic/claude-opus-4-6"},
		{"  Sonnet  ", "anthropic/claude-sonnet-4-6"},
	}

	for _, tt := range tests {
		got := ResolveModelAlias(tt.input)
		if got != tt.want {
			t.Errorf("ResolveModelAlias(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
