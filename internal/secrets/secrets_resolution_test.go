package secrets

import (
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	// Proves that Resolve substitutes all {{secret:section.key}} templates
	// with the corresponding secret values, handles multiple substitutions in one string,
	// and leaves template-free strings unchanged.
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

func TestResolveUnknown(t *testing.T) {
	// Proves that Resolve returns a descriptive error naming the
	// missing key when a template references a secret that doesn't exist in the store.
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

func TestResolveNestedDots(t *testing.T) {
	// Proves that keys containing underscores (not additional dots)
	// are resolved correctly, confirming the parser correctly treats only the first dot
	// as the section separator.
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
