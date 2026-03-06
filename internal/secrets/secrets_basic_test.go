package secrets

import (
	"path/filepath"
	"testing"
)

// TestLoadAndGet verifies basic load and retrieval of secrets.
func TestLoadAndGet(t *testing.T) {
	path := writeSecrets(t, `
[anthropic]
setup_token = "sk-ant-oat01-test"

[telegram]
bot_token = "123:ABC"

[brave]
api_key = "BSA-test"

[custom]
github_token = "ghp_test123"
openrouter_key = "sk-or-v1-test"
`)

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tests := []struct {
		name string
		want string
	}{
		{"anthropic.setup_token", "sk-ant-oat01-test"},
		{"telegram.bot_token", "123:ABC"},
		{"brave.api_key", "BSA-test"},
		{"custom.github_token", "ghp_test123"},
		{"custom.openrouter_key", "sk-or-v1-test"},
	}

	for _, tt := range tests {
		got, ok := s.Get(tt.name)
		if !ok {
			t.Errorf("Get(%q) not found", tt.name)
			continue
		}
		if got != tt.want {
			t.Errorf("Get(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}

	// Missing key
	_, ok := s.Get("nonexistent.key")
	if ok {
		t.Error("Get(nonexistent) should return false")
	}
}

// TestLoadMissing verifies empty store is returned for nonexistent file.
func TestLoadMissing(t *testing.T) {
	s, err := Load("/nonexistent/secrets.toml")
	if err != nil {
		t.Fatalf("Load missing should not error: %v", err)
	}
	if len(s.Names()) != 0 {
		t.Errorf("Names() = %v, want empty", s.Names())
	}
}

// TestLoadInvalid verifies error for invalid TOML.
func TestLoadInvalid(t *testing.T) {
	path := writeSecrets(t, "this is not valid toml [[[")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid TOML")
	}
}

// TestNames verifies Names() returns sorted unique key names.
func TestNames(t *testing.T) {
	path := writeSecrets(t, `
[anthropic]
setup_token = "x"

[custom]
b_key = "y"
a_key = "z"
`)
	s, _ := Load(path)
	names := s.Names()

	if len(names) != 3 {
		t.Fatalf("Names() len = %d, want 3", len(names))
	}
	// Should be sorted
	if names[0] != "anthropic.setup_token" || names[1] != "custom.a_key" || names[2] != "custom.b_key" {
		t.Errorf("Names() = %v", names)
	}
}

// TestSetAndSave verifies setting secrets and persisting to disk.
func TestSetAndSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.toml")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	s.Set("custom.api_key", "sk-test-123")
	s.Set("anthropic.setup_token", "sk-ant-456")

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload and verify
	s2, err := Load(path)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}

	v, ok := s2.Get("custom.api_key")
	if !ok || v != "sk-test-123" {
		t.Errorf("custom.api_key = %q, ok=%v", v, ok)
	}
	v, ok = s2.Get("anthropic.setup_token")
	if !ok || v != "sk-ant-456" {
		t.Errorf("anthropic.setup_token = %q, ok=%v", v, ok)
	}
}

// TestRemove verifies removing secrets and persisting deletions.
func TestRemove(t *testing.T) {
	path := writeSecrets(t, `
[custom]
key1 = "val1"
key2 = "val2"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !s.Remove("custom.key1") {
		t.Error("Remove should return true for existing key")
	}
	if s.Remove("custom.nonexistent") {
		t.Error("Remove should return false for missing key")
	}

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s2, err := Load(path)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	if _, ok := s2.Get("custom.key1"); ok {
		t.Error("key1 should be removed")
	}
	if _, ok := s2.Get("custom.key2"); !ok {
		t.Error("key2 should still exist")
	}
}
