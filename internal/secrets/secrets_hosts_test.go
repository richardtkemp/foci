package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadWithAllowedHosts verifies loading hosts restrictions.
func TestLoadWithAllowedHosts(t *testing.T) {
	path := writeSecrets(t, `
[anthropic]
setup_token = "sk-ant-test"
allowed_hosts = ["api.anthropic.com", "api.example.com"]

[custom]
github_token = "ghp_test123"

[locked]
api_key = "sk-locked-456"
allowed_hosts = ["api.locked.com"]
`)

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	v, ok := s.Get("anthropic.setup_token")
	if !ok || v != "sk-ant-test" {
		t.Errorf("anthropic.setup_token = %q, ok=%v", v, ok)
	}

	hosts := s.AllowedHosts("anthropic.setup_token")
	if len(hosts) != 2 || hosts[0] != "api.anthropic.com" || hosts[1] != "api.example.com" {
		t.Errorf("AllowedHosts(anthropic.setup_token) = %v", hosts)
	}

	hosts = s.AllowedHosts("custom.github_token")
	if hosts != nil {
		t.Errorf("AllowedHosts(custom.github_token) = %v, want nil", hosts)
	}
}

// TestCheckHostAllowed verifies host allowlist enforcement.
func TestCheckHostAllowed(t *testing.T) {
	path := writeSecrets(t, `
[myapi]
token = "sk-test"
allowed_hosts = ["api.example.com", "api.backup.com"]
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := s.CheckHostAllowed("myapi.token", "https://api.example.com/v1/data"); err != nil {
		t.Errorf("expected allowed, got: %v", err)
	}

	if err := s.CheckHostAllowed("myapi.token", "https://evil.com/steal"); err == nil {
		t.Error("expected error for blocked host")
	}

	if err := s.CheckHostAllowed("myapi.token", "https://api.example.com@evil.com/steal"); err == nil {
		t.Error("expected error for userinfo attack URL")
	}

	if err := s.CheckHostAllowed("myapi.token", "https://api.example.com:8443/v1/data"); err != nil {
		t.Errorf("expected allowed with port, got: %v", err)
	}

	if err := s.CheckHostAllowed("myapi.token", "https://API.EXAMPLE.COM/v1/data"); err != nil {
		t.Errorf("expected case-insensitive match, got: %v", err)
	}
}

// TestCheckHostAllowedNoHosts verifies error when secret has no allowed_hosts.
func TestCheckHostAllowedNoHosts(t *testing.T) {
	path := writeSecrets(t, `
[legacy]
token = "sk-legacy"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	err = s.CheckHostAllowed("legacy.token", "https://api.example.com/data")
	if err == nil {
		t.Error("expected error for secret without allowed_hosts")
	}
	if !strings.Contains(err.Error(), "no allowed_hosts") {
		t.Errorf("error should mention no allowed_hosts: %v", err)
	}
}

// TestSectionAllowedHosts verifies querying allowed hosts for a section.
func TestSectionAllowedHosts(t *testing.T) {
	path := writeSecrets(t, `
[myapi]
token = "sk-test"
allowed_hosts = ["api.example.com"]

[legacy]
key = "val"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	hosts := s.SectionAllowedHosts("myapi")
	if len(hosts) != 1 || hosts[0] != "api.example.com" {
		t.Errorf("SectionAllowedHosts(myapi) = %v", hosts)
	}
	if s.SectionAllowedHosts("legacy") != nil {
		t.Error("SectionAllowedHosts(legacy) should be nil")
	}
}

// TestAddAllowedHost verifies adding hosts to a section.
func TestAddAllowedHost(t *testing.T) {
	path := writeSecrets(t, `
[myapi]
token = "sk-test"
allowed_hosts = ["api.example.com"]
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	s.AddAllowedHost("myapi", "api.backup.com")
	hosts := s.SectionAllowedHosts("myapi")
	if len(hosts) != 2 {
		t.Fatalf("expected 2 hosts, got %d: %v", len(hosts), hosts)
	}

	s.AddAllowedHost("myapi", "API.EXAMPLE.COM")
	hosts = s.SectionAllowedHosts("myapi")
	if len(hosts) != 2 {
		t.Errorf("duplicate add should be no-op, got %d hosts: %v", len(hosts), hosts)
	}
}

// TestRemoveAllowedHost verifies removing hosts from a section.
func TestRemoveAllowedHost(t *testing.T) {
	path := writeSecrets(t, `
[myapi]
token = "sk-test"
allowed_hosts = ["api.example.com", "api.backup.com"]
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !s.RemoveAllowedHost("myapi", "API.EXAMPLE.COM") {
		t.Error("RemoveAllowedHost should return true for existing host")
	}
	hosts := s.SectionAllowedHosts("myapi")
	if len(hosts) != 1 || hosts[0] != "api.backup.com" {
		t.Errorf("after remove: %v", hosts)
	}

	if s.RemoveAllowedHost("myapi", "nonexistent.com") {
		t.Error("RemoveAllowedHost should return false for missing host")
	}
}

// TestSetAllowedHosts verifies replacing the full hosts list.
func TestSetAllowedHosts(t *testing.T) {
	path := writeSecrets(t, `
[myapi]
token = "sk-test"
allowed_hosts = ["old.com"]
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	s.SetAllowedHosts("myapi", []string{"new1.com", "new2.com"})
	hosts := s.SectionAllowedHosts("myapi")
	if len(hosts) != 2 || hosts[0] != "new1.com" || hosts[1] != "new2.com" {
		t.Errorf("SetAllowedHosts: %v", hosts)
	}

	s.SetAllowedHosts("myapi", nil)
	if s.SectionAllowedHosts("myapi") != nil {
		t.Error("SetAllowedHosts(nil) should clear")
	}
}

// TestAddRemoveAllowedHostsPersist verifies changes survive save/load cycle.
func TestAddRemoveAllowedHostsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.toml")
	os.WriteFile(path, []byte(`
[myapi]
token = "sk-test"
allowed_hosts = ["api.example.com"]
`), 0600)

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	s.AddAllowedHost("myapi", "api.new.com")
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s2, err := Load(path)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	hosts := s2.SectionAllowedHosts("myapi")
	if len(hosts) != 2 {
		t.Errorf("expected 2 hosts after persist, got %v", hosts)
	}
}

// TestAllowedHostsNoDot verifies localhost (no dot) is allowed in the config.
func TestAllowedHostsNoDot(t *testing.T) {
	path := writeSecrets(t, `
[myapi]
token = "sk-test"
allowed_hosts = ["localhost"]
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	hosts := s.SectionAllowedHosts("myapi")
	if len(hosts) != 1 || hosts[0] != "localhost" {
		t.Errorf("localhost host not loaded: %v", hosts)
	}
}

// TestCheckHostAllowedInvalidURL verifies error handling for invalid URLs.
func TestCheckHostAllowedInvalidURL(t *testing.T) {
	path := writeSecrets(t, `
[myapi]
token = "sk-test"
allowed_hosts = ["api.example.com"]
`)
	s, _ := Load(path)
	err := s.CheckHostAllowed("myapi.token", "not a valid url")
	if err == nil {
		t.Error("expected error for invalid URL")
	}
}

// TestAllowedHostsPerSecret verifies per-secret hosts configuration.
func TestAllowedHostsPerSecret(t *testing.T) {
	path := writeSecrets(t, `
[api_a]
token = "token_a"
allowed_hosts = ["api-a.example.com"]

[api_b]
token = "token_b"
allowed_hosts = ["api-b.example.com"]
`)
	s, _ := Load(path)

	hostsA := s.AllowedHosts("api_a.token")
	if len(hostsA) != 1 || hostsA[0] != "api-a.example.com" {
		t.Errorf("api_a hosts = %v", hostsA)
	}

	hostsB := s.AllowedHosts("api_b.token")
	if len(hostsB) != 1 || hostsB[0] != "api-b.example.com" {
		t.Errorf("api_b hosts = %v", hostsB)
	}
}

// TestCheckHostAllowedSuccess verifies successful host allowance.
func TestCheckHostAllowedSuccess(t *testing.T) {
	path := writeSecrets(t, `
[api]
token = "sk-test"
allowed_hosts = ["api.example.com"]
`)
	s, _ := Load(path)
	err := s.CheckHostAllowed("api.token", "https://api.example.com/v1/endpoint")
	if err != nil {
		t.Errorf("expected success, got error: %v", err)
	}
}

// TestCheckHostAllowedFailure verifies blocked host rejection.
func TestCheckHostAllowedFailure(t *testing.T) {
	path := writeSecrets(t, `
[api]
token = "sk-test"
allowed_hosts = ["api.example.com"]
`)
	s, _ := Load(path)
	err := s.CheckHostAllowed("api.token", "https://evil.com/endpoint")
	if err == nil {
		t.Error("expected error for disallowed host")
	}
}
