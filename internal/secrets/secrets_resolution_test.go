package secrets

import (
	"strings"
	"testing"
)

// TestResolve verifies template resolution replaces {{secret:...}} with actual values.
func TestResolve(t *testing.T) {
	path := writeSecrets(t, `
[custom]
github_token = "ghp_abc123"
api_key = "key_xyz"
`)
	s, _ := Load(path)

	tests := []struct {
		input string
		want  string
	}{
		{
			`curl -H "Authorization: Bearer {{secret:custom.github_token}}" https://api.github.com`,
			`curl -H "Authorization: Bearer ghp_abc123" https://api.github.com`,
		},
		{
			`echo {{secret:custom.api_key}}`,
			`echo key_xyz`,
		},
		{
			`no templates here`,
			`no templates here`,
		},
		{
			`{{secret:custom.github_token}} and {{secret:custom.api_key}}`,
			`ghp_abc123 and key_xyz`,
		},
	}

	for _, tt := range tests {
		got, err := s.Resolve(tt.input)
		if err != nil {
			t.Errorf("Resolve(%q) error: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("Resolve(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestResolveUnknown verifies error when resolving unknown secrets.
func TestResolveUnknown(t *testing.T) {
	path := writeSecrets(t, `[custom]
key = "val"
`)
	s, _ := Load(path)

	_, err := s.Resolve("{{secret:nonexistent.key}}")
	if err == nil {
		t.Fatal("expected error for unknown secret")
	}
	if !strings.Contains(err.Error(), "nonexistent.key") {
		t.Errorf("error = %q", err.Error())
	}
}

// TestResolveNestedDots verifies resolution works with underscored key names.
func TestResolveNestedDots(t *testing.T) {
	path := writeSecrets(t, `
[custom]
my_key = "value123"
`)
	s, _ := Load(path)

	got, err := s.Resolve("{{secret:custom.my_key}}")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "value123" {
		t.Errorf("got %q", got)
	}
}
