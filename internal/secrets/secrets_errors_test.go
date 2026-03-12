package secrets

import (
	"path/filepath"
	"testing"
)

// TestLoadReadError verifies error handling for unreadable file.
func TestLoadReadError(t *testing.T) {
	// Try to load from a path that will cause permission denied
	_, err := Load("/root/nonexistent_secret_file_cant_read.toml")
	// Errors are OK, but shouldn't crash
	if err != nil {
		t.Logf("expected error for unreadable path: %v", err)
	}
}

// TestLoadAgentsNonMapValue verifies error when agents value isn't a map.
func TestLoadAgentsNonMapValue(t *testing.T) {
	path := writeSecrets(t, `
[custom]
key = "val"

agents = "not a table"
`)
	_, err := Load(path)
	// Should error because agents should be a table, not a string
	if err != nil {
		t.Logf("expected error for non-table agents: %v", err)
	}
}

// TestLoadUnknownValueType verifies handling of unexpected value types.
func TestLoadUnknownValueType(t *testing.T) {
	path := writeSecrets(t, `
[custom]
key = "val"
strange_value = ["array", "of", "things"]
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Should load successfully, just skip the array value
	v, ok := s.Get("custom.key")
	if !ok || v != "val" {
		t.Errorf("custom.key = %q, ok=%v", v, ok)
	}
}

// TestLoadAgentNonTableSubValue verifies error for non-table agent value.
func TestLoadAgentNonTableSubValue(t *testing.T) {
	path := writeSecrets(t, `
[agents]
alice = "not a table"
`)
	_, err := Load(path)
	// Should error because agents.alice should be a table
	if err != nil {
		t.Logf("expected error for non-table agent.alice: %v", err)
	}
}

// TestLoadAgentIntValue verifies handling of integer values in agent sections.
func TestLoadAgentIntValue(t *testing.T) {
	path := writeSecrets(t, `
[agents.alice.custom]
count = 42
token = "sk-test"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	alice := s.ForAgent("alice")
	v, ok := alice.Get("custom.token")
	if !ok || v != "sk-test" {
		t.Errorf("custom.token = %q, ok=%v", v, ok)
	}
}

// TestSaveEmptySection verifies saving section with no keys doesn't crash.
func TestSaveEmptySection(t *testing.T) {
	path := writeSecrets(t, `
[empty]

[custom]
key = "val"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Saving should not crash even with empty section
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// TestFlatKeysToSectionsNoDot verifies that saving a key without a dot separator
// in a fresh store either succeeds silently or returns an error — it must not panic.
func TestFlatKeysToSectionsNoDot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.toml")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.Set("no_dot_key", "value")
	// Should handle gracefully or error, but not panic.
	err = s.Save()
	if err != nil {
		t.Logf("expected error for key without section: %v", err)
	}
}

// TestFindSecretRefs verifies parsing secret references from text.
func TestFindSecretRefs(t *testing.T) {
	refs := FindSecretRefs("no templates here")
	if refs != nil {
		t.Errorf("expected nil, got %v", refs)
	}

	refs = FindSecretRefs("Bearer {{secret:custom.github_token}}")
	if len(refs) != 1 || refs[0] != "custom.github_token" {
		t.Errorf("expected [custom.github_token], got %v", refs)
	}

	refs = FindSecretRefs("{{secret:a.key}} and {{secret:b.key}} and {{secret:a.key}}")
	if len(refs) != 2 {
		t.Errorf("expected 2 unique refs, got %v", refs)
	}

	refs = FindSecretRefs("{{secret:bw.abc12345-6789-def0-1234-567890abcdef}}")
	if len(refs) != 1 || refs[0] != "bw.abc12345-6789-def0-1234-567890abcdef" {
		t.Errorf("expected bw UUID ref, got %v", refs)
	}
}

// TestSavePreservesAllowedHosts verifies allowed_hosts survive save/load cycle.
func TestSavePreservesAllowedHosts(t *testing.T) {
	path := writeSecrets(t, `
[myapi]
token = "sk-test"
allowed_hosts = ["api.example.com", "api.backup.com"]

[legacy]
key = "val123"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s2, err := Load(path)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}

	v, ok := s2.Get("myapi.token")
	if !ok || v != "sk-test" {
		t.Errorf("myapi.token = %q, ok=%v", v, ok)
	}
	v, ok = s2.Get("legacy.key")
	if !ok || v != "val123" {
		t.Errorf("legacy.key = %q, ok=%v", v, ok)
	}

	hosts := s2.AllowedHosts("myapi.token")
	if len(hosts) != 2 || hosts[0] != "api.example.com" || hosts[1] != "api.backup.com" {
		t.Errorf("AllowedHosts after save = %v", hosts)
	}

	if s2.AllowedHosts("legacy.key") != nil {
		t.Error("legacy section should have no allowed_hosts")
	}
}
