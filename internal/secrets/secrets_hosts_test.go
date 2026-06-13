package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadWithAllowedHosts(t *testing.T) {
	// Proves that allowed_hosts arrays are loaded correctly per
	// section: sections that declare them expose the list, while sections without return nil.
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

func TestCheckHostAllowed(t *testing.T) {
	// Proves the full allowlist enforcement contract: allowed hosts
	// (including with ports and case variations) pass, unknown hosts fail, and userinfo
	// injection attacks are detected and rejected.
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

func TestCheckHostAllowedNoHosts(t *testing.T) {
	// Proves that a secret without an allowed_hosts list
	// cannot be used with CheckHostAllowed — the absence of a list is treated as a
	// restriction, not open access.
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

func TestSectionAllowedHosts(t *testing.T) {
	// Proves that SectionAllowedHosts returns the host list for
	// sections that have one, and nil for sections that don't.
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

func TestAddAllowedHost(t *testing.T) {
	// Proves that AddAllowedHost appends new hosts correctly and
	// that adding a duplicate (case-insensitively) is a no-op that doesn't grow the list.
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

func TestRemoveAllowedHost(t *testing.T) {
	// Proves that RemoveAllowedHost removes an existing host
	// case-insensitively (returning true), leaves remaining hosts intact, and returns
	// false when the target host is not in the list.
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

func TestSetAllowedHosts(t *testing.T) {
	// Proves that SetAllowedHosts replaces the entire host list
	// atomically, and that passing nil clears it completely.
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

func TestAddRemoveAllowedHostsPersist(t *testing.T) {
	// Proves that host list mutations (adding a host)
	// are durably written to disk and correctly reloaded on the next Load call.
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

func TestAllowedHostsNoDot(t *testing.T) {
	// Proves that a bare hostname like "localhost" (no dot) is
	// a valid allowed_hosts entry and is loaded without modification.
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

func TestCheckHostAllowedInvalidURL(t *testing.T) {
	// Proves that CheckHostAllowed returns an error
	// for a malformed URL rather than silently allowing or panicking.
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

func TestAllowedHostsPerSecret(t *testing.T) {
	// Proves that each section's allowed_hosts list is
	// independent — one section's list does not leak into another's.
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
