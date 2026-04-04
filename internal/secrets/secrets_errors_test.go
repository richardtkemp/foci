package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadReadError(t *testing.T) {
	// Proves that Load returns an error when the file exists but is unreadable.
	if os.Getuid() == 0 {
		t.Skip("chmod 0000 has no effect when running as root")
	}
	path := filepath.Join(t.TempDir(), "secrets.toml")
	os.WriteFile(path, []byte("[custom]\nkey = \"val\"\n"), 0000)
	defer os.Chmod(path, 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unreadable file, got nil")
	}
}

func TestLoadAgentsNonMapValue(t *testing.T) {
	// When the top-level "agents" key is a scalar instead of a TOML table,
	// the TOML library silently skips it (type mismatch for map target).
	// Load succeeds but the agents section is empty — no per-agent secrets
	// are loaded. This test documents that behavior.
	path := writeSecrets(t, `
agents = "not a table"

[custom]
key = "val"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Global secrets should still load.
	v, ok := s.Get("custom.key")
	if !ok || v != "val" {
		t.Errorf("custom.key = %q, ok=%v; expected 'val'", v, ok)
	}
	// Per-agent secrets should be empty (agents section was a scalar, not a table).
	alice := s.ForAgent("alice")
	if _, ok := alice.Get("anything"); ok {
		t.Error("expected no agent secrets when agents is a scalar")
	}
}

func TestLoadUnknownValueType(t *testing.T) {
	// Proves that Load silently skips array-valued keys rather
	// than failing, so a file with mixed types still yields the string secrets it contains.
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

func TestLoadAgentNonTableSubValue(t *testing.T) {
	// Proves that Load rejects a file where an agent entry is a scalar
	// string rather than a nested table, catching structural errors that
	// would otherwise cause the agent's secrets to silently vanish.
	path := writeSecrets(t, `
[agents]
alice = "not a table"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for agents.alice as scalar, got nil")
	}
}

func TestLoadAgentIntValue(t *testing.T) {
	// Proves that integer values in an agent section are loaded
	// without error, with string secrets in the same section still retrievable.
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

func TestSaveEmptySection(t *testing.T) {
	// Proves that Save handles sections that have no keys without
	// panicking, ensuring empty TOML sections don't break the serialization path.
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

func TestFlatKeysToSectionsNoDot(t *testing.T) {
	// Proves that a key without a "section.name" dot separator is silently
	// dropped by Save (flatKeysToSections skips it). The key can be Set and
	// retrieved via Get within the same Store instance, but does not survive
	// a save/load cycle.
	path := filepath.Join(t.TempDir(), "secrets.toml")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.Set("no_dot_key", "value")

	// Should be retrievable in-memory.
	v, ok := s.Get("no_dot_key")
	if !ok || v != "value" {
		t.Fatalf("Get before Save: got %q, ok=%v", v, ok)
	}

	// Save succeeds but drops the dotless key.
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload — the dotless key should be gone.
	s2, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if _, ok := s2.Get("no_dot_key"); ok {
		t.Error("dotless key survived save/load cycle — expected it to be dropped")
	}
}

func TestFindSecretRefs(t *testing.T) {
	// Proves that FindSecretRefs correctly extracts unique secret
	// key names from {{secret:...}} templates, including UUID-style keys, and returns
	// nil for text with no templates.
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

func TestSavePreservesAllowedHosts(t *testing.T) {
	// Proves that allowed_hosts arrays survive a full
	// save/load cycle alongside their sibling string secrets, and that sections without
	// allowed_hosts still return nil after the roundtrip.
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
