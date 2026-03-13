package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadInt64Values(t *testing.T) {
	// Proves that large integer values (e.g. OAuth expiry timestamps)
	// in TOML are loaded as their decimal string representation alongside normal string secrets.
	path := writeSecrets(t, `
[anthropic]
setup_token = "sk-ant-test"
oauth_expires_at = 1772334580401
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	v, ok := s.Get("anthropic.oauth_expires_at")
	if !ok {
		t.Fatal("oauth_expires_at not found")
	}
	if v != "1772334580401" {
		t.Errorf("oauth_expires_at = %q, want %q", v, "1772334580401")
	}

	v, ok = s.Get("anthropic.setup_token")
	if !ok || v != "sk-ant-test" {
		t.Errorf("setup_token = %q, ok=%v", v, ok)
	}
}

func TestSaveInt64Values(t *testing.T) {
	// Proves that when a value is a valid integer, Save writes it
	// as an unquoted TOML integer rather than a quoted string, preserving the original type.
	path := filepath.Join(t.TempDir(), "secrets.toml")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	s.Set("anthropic.oauth_expires_at", "1772334580401")
	s.Set("anthropic.setup_token", "sk-ant-test")

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, _ := os.ReadFile(path)
	content := string(data)
	if !strings.Contains(content, "oauth_expires_at = 1772334580401") {
		t.Errorf("expected unquoted integer in output:\n%s", content)
	}
	if !strings.Contains(content, `setup_token = "sk-ant-test"`) {
		t.Errorf("expected quoted string in output:\n%s", content)
	}
}

func TestRoundtripInt64(t *testing.T) {
	// Proves that integer values are not corrupted across a full
	// load→save→load cycle, confirming type-preserving serialization end-to-end.
	path := writeSecrets(t, `
[anthropic]
oauth_expires_at = 1772334580401
oauth_access_token = "sk-ant-oat01-test"
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

	v, ok := s2.Get("anthropic.oauth_expires_at")
	if !ok || v != "1772334580401" {
		t.Errorf("after roundtrip: oauth_expires_at = %q, ok=%v", v, ok)
	}
	v, ok = s2.Get("anthropic.oauth_access_token")
	if !ok || v != "sk-ant-oat01-test" {
		t.Errorf("after roundtrip: oauth_access_token = %q, ok=%v", v, ok)
	}
}
