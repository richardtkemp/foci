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
