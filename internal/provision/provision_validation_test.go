package provision

import (
	"testing"
)

// TestIsValidAgentID verifies agent ID validation rules: lowercase letters, numbers, hyphens only.
func TestIsValidAgentID(t *testing.T) {
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

// TestIsValidBotToken verifies bot token format: numeric ID colon alphanumeric+_-+ token.
func TestIsValidBotToken(t *testing.T) {
	tests := []struct {
		token string
		want  bool
	}{
		{"123456789:AAF-abcdefghijklmnopqrstuv", true},
		{"7894561230:ABCdefGHIjklMNOpqrSTUvwxyz_-12345", true},
		{"12345:short", false},
		{"notanumber:AAF-abcdefghijklmnopqrstuv", false},
		{"", false},
		{"just-a-string", false},
	}
	for _, tt := range tests {
		got := IsValidBotToken(tt.token)
		if got != tt.want {
			t.Errorf("IsValidBotToken(%q) = %v, want %v", tt.token, got, tt.want)
		}
	}
}

// TestIsValidUserID verifies user ID validation: numeric, at least 3 digits.
func TestIsValidUserID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"12345678", true},
		{"123", true},
		{"12", false},
		{"", false},
		{"abc", false},
	}
	for _, tt := range tests {
		got := IsValidUserID(tt.id)
		if got != tt.want {
			t.Errorf("IsValidUserID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

// TestResolveModelAlias verifies model alias resolution for opus, sonnet, haiku shorthands.
func TestResolveModelAlias(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"opus", "claude-opus-4-6"},
		{"sonnet", "claude-sonnet-4-6"},
		{"haiku", "claude-haiku-4-5-20251001"},
		{"", "claude-sonnet-4-6"},
		{"claude-custom-model", "claude-custom-model"},
		{"OPUS", "claude-opus-4-6"},
		{"  Sonnet  ", "claude-sonnet-4-6"},
	}

	for _, tt := range tests {
		got := ResolveModelAlias(tt.input)
		if got != tt.want {
			t.Errorf("ResolveModelAlias(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
