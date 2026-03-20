package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsAllowedInBodyDefault(t *testing.T) {
	// Proves that IsAllowedInBody returns false by default when no
	// allowed_in_body is configured for a section.
	path := writeSecrets(t, `
[custom]
api_key = "sk-test"
allowed_hosts = ["api.example.com"]
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.IsAllowedInBody("custom.api_key") {
		t.Error("IsAllowedInBody should return false by default")
	}
}

func TestIsAllowedInBodyListed(t *testing.T) {
	// Proves that IsAllowedInBody returns true for keys listed in
	// allowed_in_body for their section.
	path := writeSecrets(t, `
[custom]
api_key = "sk-test"
other_key = "sk-other"
allowed_hosts = ["api.example.com"]
allowed_in_body = ["api_key"]
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !s.IsAllowedInBody("custom.api_key") {
		t.Error("IsAllowedInBody should return true for listed key")
	}
}

func TestIsAllowedInBodyUnlistedInSameSection(t *testing.T) {
	// Proves that keys NOT in allowed_in_body return false even when other
	// keys in the same section are listed.
	path := writeSecrets(t, `
[custom]
api_key = "sk-test"
other_key = "sk-other"
allowed_in_body = ["api_key"]
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.IsAllowedInBody("custom.other_key") {
		t.Error("IsAllowedInBody should return false for unlisted key in same section")
	}
}

func TestSectionAllowedInBody(t *testing.T) {
	// Proves that SectionAllowedInBody returns the list for configured
	// sections and nil for unconfigured ones.
	path := writeSecrets(t, `
[custom]
api_key = "sk-test"
allowed_in_body = ["api_key", "other_key"]

[legacy]
key = "val"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	keys := s.SectionAllowedInBody("custom")
	if len(keys) != 2 || keys[0] != "api_key" || keys[1] != "other_key" {
		t.Errorf("SectionAllowedInBody(custom) = %v", keys)
	}
	if s.SectionAllowedInBody("legacy") != nil {
		t.Error("SectionAllowedInBody(legacy) should be nil")
	}
}

func TestAddRemoveAllowedInBody(t *testing.T) {
	// Proves that AddAllowedInBody appends keys, is a no-op for duplicates,
	// and RemoveAllowedInBody removes keys and cleans up empty slices.
	path := writeSecrets(t, `
[custom]
api_key = "sk-test"
other_key = "sk-other"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	s.AddAllowedInBody("custom", "api_key")
	keys := s.SectionAllowedInBody("custom")
	if len(keys) != 1 || keys[0] != "api_key" {
		t.Errorf("after add: %v", keys)
	}

	// Duplicate add is no-op
	s.AddAllowedInBody("custom", "api_key")
	if len(s.SectionAllowedInBody("custom")) != 1 {
		t.Error("duplicate add should be no-op")
	}

	s.AddAllowedInBody("custom", "other_key")
	if len(s.SectionAllowedInBody("custom")) != 2 {
		t.Error("expected 2 keys after second add")
	}

	if !s.RemoveAllowedInBody("custom", "api_key") {
		t.Error("RemoveAllowedInBody should return true for existing key")
	}
	keys = s.SectionAllowedInBody("custom")
	if len(keys) != 1 || keys[0] != "other_key" {
		t.Errorf("after remove: %v", keys)
	}

	if s.RemoveAllowedInBody("custom", "nonexistent") {
		t.Error("RemoveAllowedInBody should return false for missing key")
	}

	// Remove last key should clean up
	s.RemoveAllowedInBody("custom", "other_key")
	if s.SectionAllowedInBody("custom") != nil {
		t.Error("section should be removed when empty")
	}
}

func TestSetAllowedInBody(t *testing.T) {
	// Proves that SetAllowedInBody replaces the list atomically
	// and that passing nil clears it completely.
	path := writeSecrets(t, `
[custom]
api_key = "sk-test"
allowed_in_body = ["old_key"]
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	s.SetAllowedInBody("custom", []string{"new_key1", "new_key2"})
	keys := s.SectionAllowedInBody("custom")
	if len(keys) != 2 || keys[0] != "new_key1" || keys[1] != "new_key2" {
		t.Errorf("SetAllowedInBody: %v", keys)
	}

	s.SetAllowedInBody("custom", nil)
	if s.SectionAllowedInBody("custom") != nil {
		t.Error("SetAllowedInBody(nil) should clear")
	}
}

func TestAllowedInBodySaveLoad(t *testing.T) {
	// Proves that allowed_in_body round-trips correctly through Save()/Load().
	path := filepath.Join(t.TempDir(), "secrets.toml")
	os.WriteFile(path, []byte(`
[custom]
api_key = "sk-test"
other_key = "sk-other"
allowed_hosts = ["api.example.com"]
allowed_in_body = ["api_key"]
`), 0600)

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

	if !s2.IsAllowedInBody("custom.api_key") {
		t.Error("api_key should be allowed in body after roundtrip")
	}
	if s2.IsAllowedInBody("custom.other_key") {
		t.Error("other_key should not be allowed in body after roundtrip")
	}
}

func TestForAgentAllowedInBody(t *testing.T) {
	// Proves that ForAgent propagates global allowed_in_body and correctly
	// overlays agent-specific allowed_in_body settings.
	path := writeSecrets(t, `
[custom]
api_key = "sk-global"
other_key = "sk-other"
allowed_in_body = ["api_key"]

[agents.alpha.custom]
api_key = "sk-alpha"
allowed_in_body = ["api_key", "other_key"]
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Global: only api_key allowed
	if !s.IsAllowedInBody("custom.api_key") {
		t.Error("global: api_key should be allowed in body")
	}
	if s.IsAllowedInBody("custom.other_key") {
		t.Error("global: other_key should not be allowed in body")
	}

	// Agent alpha: both keys allowed (overridden)
	as := s.ForAgent("alpha")
	if !as.IsAllowedInBody("custom.api_key") {
		t.Error("alpha: api_key should be allowed in body")
	}
	if !as.IsAllowedInBody("custom.other_key") {
		t.Error("alpha: other_key should be allowed in body (agent override)")
	}

	// Agent beta (no overrides): inherits global
	bs := s.ForAgent("beta")
	if !bs.IsAllowedInBody("custom.api_key") {
		t.Error("beta: api_key should be allowed in body (inherited)")
	}
	if bs.IsAllowedInBody("custom.other_key") {
		t.Error("beta: other_key should not be allowed in body")
	}
}

func TestForAgentAllowedInBodyFiltered(t *testing.T) {
	// Proves that allowed_in_body for a restricted section is not visible
	// to agents that are denied access to that section.
	path := writeSecrets(t, `
[restricted]
api_key = "sk-restricted"
denied_agents = ["blocked"]
allowed_in_body = ["api_key"]
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Allowed agent sees allowed_in_body
	as := s.ForAgent("allowed")
	if !as.IsAllowedInBody("restricted.api_key") {
		t.Error("allowed agent should see allowed_in_body")
	}

	// Blocked agent does not
	bs := s.ForAgent("blocked")
	if bs.IsAllowedInBody("restricted.api_key") {
		t.Error("blocked agent should not see allowed_in_body")
	}
}

func TestSavePreservesAllowedInBody(t *testing.T) {
	// Proves that agent-specific allowed_in_body sections are preserved
	// through a save/load roundtrip.
	path := filepath.Join(t.TempDir(), "secrets.toml")
	os.WriteFile(path, []byte(`
[custom]
api_key = "sk-global"
allowed_in_body = ["api_key"]

[agents.fotini.custom]
api_key = "sk-fotini"
allowed_in_body = ["api_key"]
`), 0600)

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify file contains allowed_in_body
	data, _ := os.ReadFile(path)
	content := string(data)
	if !strings.Contains(content, "allowed_in_body") {
		t.Error("saved file should contain allowed_in_body")
	}

	s2, err := Load(path)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}

	// Global
	if !s2.IsAllowedInBody("custom.api_key") {
		t.Error("global allowed_in_body not preserved")
	}

	// Agent
	fs := s2.ForAgent("fotini")
	if !fs.IsAllowedInBody("custom.api_key") {
		t.Error("agent allowed_in_body not preserved")
	}
}

func TestIsAllowedInBodyBareKey(t *testing.T) {
	// Proves that IsAllowedInBody handles a name without a dot gracefully
	// (returns false, no panic).
	path := writeSecrets(t, `
[custom]
api_key = "sk-test"
allowed_in_body = ["api_key"]
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if s.IsAllowedInBody("nodot") {
		t.Error("bare key without dot should return false")
	}
}
