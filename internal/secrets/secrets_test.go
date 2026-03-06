package secrets

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestGeneratePassphrase(t *testing.T) {
	// Basic generation
	p, err := GeneratePassphrase(5)
	if err != nil {
		t.Fatalf("GeneratePassphrase(5): %v", err)
	}

	words := strings.Split(p, "-")
	if len(words) != 5 {
		t.Errorf("expected 5 words, got %d: %q", len(words), p)
	}

	// All words should be from the wordlist
	wordSet := make(map[string]bool, len(effShortWordlist))
	for _, w := range effShortWordlist {
		wordSet[w] = true
	}
	for _, w := range words {
		if !wordSet[w] {
			t.Errorf("word %q not in EFF wordlist", w)
		}
	}

	// Single word
	p1, err := GeneratePassphrase(1)
	if err != nil {
		t.Fatalf("GeneratePassphrase(1): %v", err)
	}
	if strings.Contains(p1, "-") {
		t.Errorf("single word should have no hyphens: %q", p1)
	}
	if !wordSet[p1] {
		t.Errorf("single word %q not in wordlist", p1)
	}

	// Zero or negative word count
	if _, err := GeneratePassphrase(0); err == nil {
		t.Error("expected error for wordCount=0")
	}
	if _, err := GeneratePassphrase(-1); err == nil {
		t.Error("expected error for wordCount=-1")
	}

	// Uniqueness: two calls should (almost certainly) produce different results
	p2, _ := GeneratePassphrase(5)
	p3, _ := GeneratePassphrase(5)
	if p2 == p3 {
		t.Errorf("two consecutive passphrases are identical: %q (extremely unlikely)", p2)
	}
}

func TestEFFWordlistSize(t *testing.T) {
	if len(effShortWordlist) != 1296 {
		t.Errorf("EFF short wordlist has %d words, expected 1296", len(effShortWordlist))
	}
}

func writeSecrets(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.toml")
	os.WriteFile(path, []byte(content), 0600)
	return path
}

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

func TestLoadMissing(t *testing.T) {
	s, err := Load("/nonexistent/secrets.toml")
	if err != nil {
		t.Fatalf("Load missing should not error: %v", err)
	}
	if len(s.Names()) != 0 {
		t.Errorf("Names() = %v, want empty", s.Names())
	}
}

func TestLoadInvalid(t *testing.T) {
	path := writeSecrets(t, "this is not valid toml [[[")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid TOML")
	}
}

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

func TestRedact(t *testing.T) {
	path := writeSecrets(t, `
[anthropic]
setup_token = "sk-ant-oat01-supersecret"

[custom]
api_key = "BSA-mykey123"
`)
	s, _ := Load(path)

	input := `Config dump:
ANTHROPIC_TOKEN=sk-ant-oat01-supersecret
API_KEY=BSA-mykey123
other stuff`

	result := s.Redact(input)

	if strings.Contains(result, "sk-ant-oat01-supersecret") {
		t.Error("token not redacted")
	}
	if strings.Contains(result, "BSA-mykey123") {
		t.Error("api_key not redacted")
	}
	if !strings.Contains(result, "[REDACTED]") {
		t.Error("missing [REDACTED] placeholder")
	}
	if !strings.Contains(result, "other stuff") {
		t.Error("non-secret text was modified")
	}
}

func TestRedactShortValues(t *testing.T) {
	path := writeSecrets(t, `
[custom]
short = "ab"
long = "longersecret123"
`)
	s, _ := Load(path)

	input := "ab is fine, longersecret123 is not"
	result := s.Redact(input)

	// Short value "ab" should NOT be redacted (< 4 chars, too many false positives)
	if !strings.Contains(result, "ab is fine") {
		t.Errorf("short value was redacted: %q", result)
	}
	// Long value should be redacted
	if strings.Contains(result, "longersecret123") {
		t.Error("long value not redacted")
	}
}

func TestRedactEmpty(t *testing.T) {
	s, _ := Load("/nonexistent")
	result := s.Redact("nothing to redact")
	if result != "nothing to redact" {
		t.Errorf("result = %q", result)
	}
}

func TestIsBlockedPath(t *testing.T) {
	path := writeSecrets(t, `[custom]
key = "val"
`)
	s, _ := Load(path)

	if !s.IsBlockedPath("secrets.toml") {
		t.Error("secrets.toml should be blocked")
	}
	if !s.IsBlockedPath("/home/user/secrets.toml") {
		t.Error("full path to secrets.toml should be blocked")
	}
	if !s.IsBlockedPath("/proc/self/environ") {
		t.Error("/proc/self/environ should be blocked")
	}
	if s.IsBlockedPath("/home/user/code.go") {
		t.Error("code.go should not be blocked")
	}
}

func TestIsBlockedCommand(t *testing.T) {
	path := writeSecrets(t, `[custom]
key = "val"
`)
	s, _ := Load(path)

	if !s.IsBlockedCommand("cat secrets.toml") {
		t.Error("cat secrets.toml should be blocked")
	}
	if !s.IsBlockedCommand("cat /proc/self/environ") {
		t.Error("cat /proc/self/environ should be blocked")
	}
	if s.IsBlockedCommand("echo hello") {
		t.Error("echo hello should not be blocked")
	}
}

func TestAddBlockedPaths(t *testing.T) {
	s, _ := Load("/nonexistent")
	s.AddBlockedPaths([]string{".env", "credentials.json"})

	if !s.IsBlockedPath(".env") {
		t.Error(".env should be blocked after adding")
	}
	if !s.IsBlockedPath("credentials.json") {
		t.Error("credentials.json should be blocked after adding")
	}
}

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

func TestLoadInt64Values(t *testing.T) {
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

	// String values should still work
	v, ok = s.Get("anthropic.setup_token")
	if !ok || v != "sk-ant-test" {
		t.Errorf("setup_token = %q, ok=%v", v, ok)
	}
}

func TestSaveInt64Values(t *testing.T) {
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

	// Read raw file — integer should be unquoted
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

	// Reload and verify
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

	// Values should still load correctly
	v, ok := s.Get("anthropic.setup_token")
	if !ok || v != "sk-ant-test" {
		t.Errorf("anthropic.setup_token = %q, ok=%v", v, ok)
	}
	v, ok = s.Get("custom.github_token")
	if !ok || v != "ghp_test123" {
		t.Errorf("custom.github_token = %q, ok=%v", v, ok)
	}
	v, ok = s.Get("locked.api_key")
	if !ok || v != "sk-locked-456" {
		t.Errorf("locked.api_key = %q, ok=%v", v, ok)
	}

	// AllowedHosts should return correct lists
	hosts := s.AllowedHosts("anthropic.setup_token")
	if len(hosts) != 2 || hosts[0] != "api.anthropic.com" || hosts[1] != "api.example.com" {
		t.Errorf("AllowedHosts(anthropic.setup_token) = %v", hosts)
	}

	hosts = s.AllowedHosts("locked.api_key")
	if len(hosts) != 1 || hosts[0] != "api.locked.com" {
		t.Errorf("AllowedHosts(locked.api_key) = %v", hosts)
	}

	// Legacy section without allowed_hosts returns nil
	hosts = s.AllowedHosts("custom.github_token")
	if hosts != nil {
		t.Errorf("AllowedHosts(custom.github_token) = %v, want nil", hosts)
	}
}

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

	// Allowed host
	if err := s.CheckHostAllowed("myapi.token", "https://api.example.com/v1/data"); err != nil {
		t.Errorf("expected allowed, got: %v", err)
	}

	// Blocked host
	if err := s.CheckHostAllowed("myapi.token", "https://evil.com/steal"); err == nil {
		t.Error("expected error for blocked host")
	}

	// Userinfo attack: hostname should be evil.com, not api.example.com
	if err := s.CheckHostAllowed("myapi.token", "https://api.example.com@evil.com/steal"); err == nil {
		t.Error("expected error for userinfo attack URL")
	}

	// Port handling — hostname should strip port
	if err := s.CheckHostAllowed("myapi.token", "https://api.example.com:8443/v1/data"); err != nil {
		t.Errorf("expected allowed with port, got: %v", err)
	}

	// Case-insensitive comparison (RFC 4343)
	if err := s.CheckHostAllowed("myapi.token", "https://API.EXAMPLE.COM/v1/data"); err != nil {
		t.Errorf("expected case-insensitive match, got: %v", err)
	}
}

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

func TestFindSecretRefs(t *testing.T) {
	// No templates
	refs := FindSecretRefs("no templates here")
	if refs != nil {
		t.Errorf("expected nil, got %v", refs)
	}

	// Single template
	refs = FindSecretRefs("Bearer {{secret:custom.github_token}}")
	if len(refs) != 1 || refs[0] != "custom.github_token" {
		t.Errorf("expected [custom.github_token], got %v", refs)
	}

	// Multiple templates (including duplicates)
	refs = FindSecretRefs("{{secret:a.key}} and {{secret:b.key}} and {{secret:a.key}}")
	if len(refs) != 2 {
		t.Errorf("expected 2 unique refs, got %v", refs)
	}

	// UUID-style key with hyphens (bitwarden)
	refs = FindSecretRefs("{{secret:bw.abc12345-6789-def0-1234-567890abcdef}}")
	if len(refs) != 1 || refs[0] != "bw.abc12345-6789-def0-1234-567890abcdef" {
		t.Errorf("expected bw UUID ref, got %v", refs)
	}
}

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

	// Reload and verify
	s2, err := Load(path)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}

	// Values preserved
	v, ok := s2.Get("myapi.token")
	if !ok || v != "sk-test" {
		t.Errorf("myapi.token = %q, ok=%v", v, ok)
	}
	v, ok = s2.Get("legacy.key")
	if !ok || v != "val123" {
		t.Errorf("legacy.key = %q, ok=%v", v, ok)
	}

	// AllowedHosts preserved
	hosts := s2.AllowedHosts("myapi.token")
	if len(hosts) != 2 || hosts[0] != "api.example.com" || hosts[1] != "api.backup.com" {
		t.Errorf("AllowedHosts after save = %v", hosts)
	}

	// Legacy section still has no allowed_hosts
	if s2.AllowedHosts("legacy.key") != nil {
		t.Error("legacy section should have no allowed_hosts")
	}
}

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
	if s.SectionAllowedHosts("nonexistent") != nil {
		t.Error("SectionAllowedHosts(nonexistent) should be nil")
	}
}

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

	// Add new host
	s.AddAllowedHost("myapi", "api.backup.com")
	hosts := s.SectionAllowedHosts("myapi")
	if len(hosts) != 2 {
		t.Fatalf("expected 2 hosts, got %d: %v", len(hosts), hosts)
	}

	// Add duplicate (case insensitive) — should be no-op
	s.AddAllowedHost("myapi", "API.EXAMPLE.COM")
	hosts = s.SectionAllowedHosts("myapi")
	if len(hosts) != 2 {
		t.Errorf("duplicate add should be no-op, got %d hosts: %v", len(hosts), hosts)
	}

	// Add to section with no existing hosts
	s.AddAllowedHost("legacy", "api.new.com")
	hosts = s.SectionAllowedHosts("legacy")
	if len(hosts) != 1 || hosts[0] != "api.new.com" {
		t.Errorf("SectionAllowedHosts(legacy) = %v", hosts)
	}

	// Add empty host — should be no-op
	s.AddAllowedHost("myapi", "")
	if len(s.SectionAllowedHosts("myapi")) != 2 {
		t.Error("empty host should be no-op")
	}

	// Normalize to lowercase
	s.AddAllowedHost("myapi", "API.UPPER.COM")
	hosts = s.SectionAllowedHosts("myapi")
	found := false
	for _, h := range hosts {
		if h == "api.upper.com" {
			found = true
		}
	}
	if !found {
		t.Errorf("host should be normalized to lowercase: %v", hosts)
	}
}

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

	// Remove existing host (case insensitive)
	if !s.RemoveAllowedHost("myapi", "API.EXAMPLE.COM") {
		t.Error("RemoveAllowedHost should return true for existing host")
	}
	hosts := s.SectionAllowedHosts("myapi")
	if len(hosts) != 1 || hosts[0] != "api.backup.com" {
		t.Errorf("after remove: %v", hosts)
	}

	// Remove nonexistent host
	if s.RemoveAllowedHost("myapi", "nonexistent.com") {
		t.Error("RemoveAllowedHost should return false for missing host")
	}

	// Remove last host — section should be cleaned up
	if !s.RemoveAllowedHost("myapi", "api.backup.com") {
		t.Error("RemoveAllowedHost should return true")
	}
	if s.SectionAllowedHosts("myapi") != nil {
		t.Error("section with no hosts should return nil")
	}

	// Remove from nonexistent section
	if s.RemoveAllowedHost("nosection", "host.com") {
		t.Error("RemoveAllowedHost should return false for missing section")
	}
}

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

	// Replace hosts
	s.SetAllowedHosts("myapi", []string{"new1.com", "new2.com"})
	hosts := s.SectionAllowedHosts("myapi")
	if len(hosts) != 2 || hosts[0] != "new1.com" || hosts[1] != "new2.com" {
		t.Errorf("SetAllowedHosts: %v", hosts)
	}

	// Clear hosts
	s.SetAllowedHosts("myapi", nil)
	if s.SectionAllowedHosts("myapi") != nil {
		t.Error("SetAllowedHosts(nil) should clear")
	}

	// Set on new section
	s.SetAllowedHosts("newsec", []string{"host.com"})
	hosts = s.SectionAllowedHosts("newsec")
	if len(hosts) != 1 {
		t.Errorf("SetAllowedHosts new section: %v", hosts)
	}
}

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

func TestCheckSecurityMissingFile(t *testing.T) {
	s, _ := Load("/nonexistent/secrets.toml")
	warnings := s.CheckSecurity()
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for missing file, got: %v", warnings)
	}
}

func TestCheckSecurityEmptyPath(t *testing.T) {
	s := &Store{path: ""}
	warnings := s.CheckSecurity()
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for empty path, got: %v", warnings)
	}
}

func TestCheckSecurityBadPermissions(t *testing.T) {
	// Create a file with wrong permissions (not 0660)
	path := writeSecrets(t, `[custom]
key = "val"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	warnings := s.CheckSecurity()
	// Should get warnings about owner (not root), group, and permissions
	if len(warnings) == 0 {
		t.Error("expected warnings for non-root owned file with wrong permissions")
	}

	// Check that warnings mention specific issues
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "owner") && !strings.Contains(joined, "uid") {
		t.Errorf("expected owner warning in: %s", joined)
	}
	if !strings.Contains(joined, "permission") || !strings.Contains(joined, "0660") {
		t.Errorf("expected permissions warning in: %s", joined)
	}
}

func TestCheckSecurityGroupName(t *testing.T) {
	if SecurityGroupName != "foci-secrets" {
		t.Errorf("SecurityGroupName = %q, want foci-secrets", SecurityGroupName)
	}
}

func TestLoadPerAgentSecrets(t *testing.T) {
	path := writeSecrets(t, `
[anthropic]
setup_token = "sk-global"

[custom]
github_token = "ghp_global"

[agents.fotini.custom]
github_token = "ghp_fotini"
deploy_key = "dk_fotini"

[agents.fotini.myapi]
token = "sk-fotini-api"
allowed_hosts = ["api.fotini.com"]
`)

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Global values still accessible on root store
	v, ok := s.Get("anthropic.setup_token")
	if !ok || v != "sk-global" {
		t.Errorf("global anthropic.setup_token = %q, ok=%v", v, ok)
	}

	// ForAgent returns merged view
	fs := s.ForAgent("fotini")

	// Agent override wins
	v, ok = fs.Get("custom.github_token")
	if !ok || v != "ghp_fotini" {
		t.Errorf("fotini custom.github_token = %q, ok=%v", v, ok)
	}

	// Agent-only key visible
	v, ok = fs.Get("custom.deploy_key")
	if !ok || v != "dk_fotini" {
		t.Errorf("fotini custom.deploy_key = %q, ok=%v", v, ok)
	}

	// Global fallback
	v, ok = fs.Get("anthropic.setup_token")
	if !ok || v != "sk-global" {
		t.Errorf("fotini anthropic.setup_token = %q, ok=%v", v, ok)
	}

	// Agent-specific allowed_hosts
	hosts := fs.AllowedHosts("myapi.token")
	if len(hosts) != 1 || hosts[0] != "api.fotini.com" {
		t.Errorf("fotini AllowedHosts(myapi.token) = %v", hosts)
	}
}

func TestForAgentOverridesGlobal(t *testing.T) {
	path := writeSecrets(t, `
[custom]
api_key = "global_key"

[agents.alpha.custom]
api_key = "alpha_key"
`)
	s, _ := Load(path)
	as := s.ForAgent("alpha")

	v, _ := as.Get("custom.api_key")
	if v != "alpha_key" {
		t.Errorf("expected alpha_key, got %q", v)
	}

	// Root store still has global
	v, _ = s.Get("custom.api_key")
	if v != "global_key" {
		t.Errorf("expected global_key on root, got %q", v)
	}
}

func TestForAgentFallbackToGlobal(t *testing.T) {
	path := writeSecrets(t, `
[anthropic]
setup_token = "sk-global"

[custom]
key_a = "val_a"
key_b = "val_b"

[agents.beta.custom]
key_a = "beta_a"
`)
	s, _ := Load(path)
	bs := s.ForAgent("beta")

	// Overridden
	v, _ := bs.Get("custom.key_a")
	if v != "beta_a" {
		t.Errorf("expected beta_a, got %q", v)
	}
	// Fallback to global
	v, _ = bs.Get("custom.key_b")
	if v != "val_b" {
		t.Errorf("expected val_b, got %q", v)
	}
	v, _ = bs.Get("anthropic.setup_token")
	if v != "sk-global" {
		t.Errorf("expected sk-global, got %q", v)
	}
}

func TestForAgentIsolation(t *testing.T) {
	path := writeSecrets(t, `
[custom]
shared = "global"

[agents.alice.custom]
private = "alice_secret"

[agents.bob.custom]
private = "bob_secret"
`)
	s, _ := Load(path)
	alice := s.ForAgent("alice")
	bob := s.ForAgent("bob")

	// Alice can't see Bob's secret
	v, ok := alice.Get("custom.private")
	if !ok || v != "alice_secret" {
		t.Errorf("alice custom.private = %q, ok=%v", v, ok)
	}
	v, ok = bob.Get("custom.private")
	if !ok || v != "bob_secret" {
		t.Errorf("bob custom.private = %q, ok=%v", v, ok)
	}

	// Both see shared
	v, _ = alice.Get("custom.shared")
	if v != "global" {
		t.Errorf("alice custom.shared = %q", v)
	}
	v, _ = bob.Get("custom.shared")
	if v != "global" {
		t.Errorf("bob custom.shared = %q", v)
	}
}

func TestForAgentNames(t *testing.T) {
	path := writeSecrets(t, `
[anthropic]
setup_token = "sk-global"

[custom]
key = "val"

[agents.gamma.custom]
extra = "extra_val"
`)
	s, _ := Load(path)
	gs := s.ForAgent("gamma")
	names := gs.Names()

	expected := []string{"anthropic.setup_token", "custom.extra", "custom.key"}
	if len(names) != len(expected) {
		t.Fatalf("Names() = %v, want %v", names, expected)
	}
	for i, name := range names {
		if name != expected[i] {
			t.Errorf("Names()[%d] = %q, want %q", i, name, expected[i])
		}
	}
}

func TestForAgentResolve(t *testing.T) {
	path := writeSecrets(t, `
[custom]
token = "global_tok"

[agents.delta.custom]
token = "delta_tok"
`)
	s, _ := Load(path)
	ds := s.ForAgent("delta")

	got, err := ds.Resolve("Bearer {{secret:custom.token}}")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "Bearer delta_tok" {
		t.Errorf("Resolve = %q, want %q", got, "Bearer delta_tok")
	}

	// Global store still resolves to global
	got, err = s.Resolve("Bearer {{secret:custom.token}}")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "Bearer global_tok" {
		t.Errorf("Resolve on root = %q, want %q", got, "Bearer global_tok")
	}
}

func TestForAgentRedact(t *testing.T) {
	path := writeSecrets(t, `
[custom]
global_key = "supersecretglobal"

[agents.echo.custom]
agent_key = "supersecretagent"
`)
	s, _ := Load(path)
	es := s.ForAgent("echo")

	input := "data: supersecretglobal and supersecretagent here"
	result := es.Redact(input)

	if strings.Contains(result, "supersecretglobal") {
		t.Error("global secret not redacted")
	}
	if strings.Contains(result, "supersecretagent") {
		t.Error("agent secret not redacted")
	}
	if !strings.Contains(result, "[REDACTED]") {
		t.Error("missing [REDACTED]")
	}
}

func TestForAgentNoSection(t *testing.T) {
	path := writeSecrets(t, `
[anthropic]
setup_token = "sk-global"

[custom]
key = "val"
`)
	s, _ := Load(path)

	// Agent with no section gets all globals
	ns := s.ForAgent("nonexistent")

	v, ok := ns.Get("anthropic.setup_token")
	if !ok || v != "sk-global" {
		t.Errorf("nonexistent agent anthropic.setup_token = %q, ok=%v", v, ok)
	}
	v, ok = ns.Get("custom.key")
	if !ok || v != "val" {
		t.Errorf("nonexistent agent custom.key = %q, ok=%v", v, ok)
	}

	names := ns.Names()
	if len(names) != 2 {
		t.Errorf("Names() = %v, want 2 items", names)
	}
}

func TestLoadPerAgentBackwardCompat(t *testing.T) {
	// Existing secrets.toml without [agents.*] works unchanged
	path := writeSecrets(t, `
[anthropic]
setup_token = "sk-ant-test"

[custom]
github_token = "ghp_test"
allowed_hosts = ["api.github.com"]
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	v, ok := s.Get("anthropic.setup_token")
	if !ok || v != "sk-ant-test" {
		t.Errorf("anthropic.setup_token = %q, ok=%v", v, ok)
	}
	v, ok = s.Get("custom.github_token")
	if !ok || v != "ghp_test" {
		t.Errorf("custom.github_token = %q, ok=%v", v, ok)
	}
	hosts := s.AllowedHosts("custom.github_token")
	if len(hosts) != 1 || hosts[0] != "api.github.com" {
		t.Errorf("AllowedHosts = %v", hosts)
	}

	// ForAgent on a store with no agent sections still works
	fs := s.ForAgent("anyagent")
	v, ok = fs.Get("anthropic.setup_token")
	if !ok || v != "sk-ant-test" {
		t.Errorf("ForAgent anthropic.setup_token = %q, ok=%v", v, ok)
	}
}

func TestSavePreservesAgentSections(t *testing.T) {
	path := writeSecrets(t, `
[anthropic]
setup_token = "sk-global"

[agents.fotini.custom]
api_key = "fotini_key"
allowed_hosts = ["api.fotini.com"]
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload and verify
	s2, err := Load(path)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}

	// Global preserved
	v, ok := s2.Get("anthropic.setup_token")
	if !ok || v != "sk-global" {
		t.Errorf("anthropic.setup_token = %q, ok=%v", v, ok)
	}

	// Agent values preserved through save/load
	fs := s2.ForAgent("fotini")
	v, ok = fs.Get("custom.api_key")
	if !ok || v != "fotini_key" {
		t.Errorf("fotini custom.api_key = %q, ok=%v", v, ok)
	}

	// Agent allowed_hosts preserved
	hosts := fs.AllowedHosts("custom.api_key")
	if len(hosts) != 1 || hosts[0] != "api.fotini.com" {
		t.Errorf("fotini AllowedHosts = %v", hosts)
	}
}

func TestAllowedAgentsWhitelist(t *testing.T) {
	path := writeSecrets(t, `
[shared_api]
token = "shared_token"
allowed_agents = ["alice", "bob"]

[open]
key = "open_key"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	alice := s.ForAgent("alice")
	bob := s.ForAgent("bob")
	charlie := s.ForAgent("charlie")

	// Alice and Bob see shared_api
	v, ok := alice.Get("shared_api.token")
	if !ok || v != "shared_token" {
		t.Errorf("alice shared_api.token = %q, ok=%v", v, ok)
	}
	v, ok = bob.Get("shared_api.token")
	if !ok || v != "shared_token" {
		t.Errorf("bob shared_api.token = %q, ok=%v", v, ok)
	}

	// Charlie does not
	_, ok = charlie.Get("shared_api.token")
	if ok {
		t.Error("charlie should not see shared_api.token")
	}

	// All see open section
	for _, name := range []string{"alice", "bob", "charlie"} {
		as := s.ForAgent(name)
		v, ok := as.Get("open.key")
		if !ok || v != "open_key" {
			t.Errorf("%s open.key = %q, ok=%v", name, v, ok)
		}
	}
}

func TestDeniedAgentsBlacklist(t *testing.T) {
	path := writeSecrets(t, `
[internal]
token = "internal_token"
denied_agents = ["untrusted"]

[public]
key = "public_key"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	trusted := s.ForAgent("trusted")
	untrusted := s.ForAgent("untrusted")

	// Trusted sees internal
	v, ok := trusted.Get("internal.token")
	if !ok || v != "internal_token" {
		t.Errorf("trusted internal.token = %q, ok=%v", v, ok)
	}

	// Untrusted does not
	_, ok = untrusted.Get("internal.token")
	if ok {
		t.Error("untrusted should not see internal.token")
	}

	// Both see public
	v, ok = trusted.Get("public.key")
	if !ok || v != "public_key" {
		t.Errorf("trusted public.key = %q, ok=%v", v, ok)
	}
	v, ok = untrusted.Get("public.key")
	if !ok || v != "public_key" {
		t.Errorf("untrusted public.key = %q, ok=%v", v, ok)
	}
}

func TestBothAllowedAndDeniedError(t *testing.T) {
	path := writeSecrets(t, `
[broken]
token = "val"
allowed_agents = ["alice"]
denied_agents = ["bob"]
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error when both allowed_agents and denied_agents are set")
	}
	if !strings.Contains(err.Error(), "both allowed_agents and denied_agents") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestAgentOverrideSurvivesDeny(t *testing.T) {
	path := writeSecrets(t, `
[custom]
global_key = "global_val"
denied_agents = ["alice"]

[agents.alice.custom]
agent_key = "alice_val"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	alice := s.ForAgent("alice")

	// Global custom.global_key denied to alice
	_, ok := alice.Get("custom.global_key")
	if ok {
		t.Error("alice should not see custom.global_key (denied)")
	}

	// But alice's own agent override survives
	v, ok := alice.Get("custom.agent_key")
	if !ok || v != "alice_val" {
		t.Errorf("alice custom.agent_key = %q, ok=%v", v, ok)
	}
}

func TestNoRestrictionsDefault(t *testing.T) {
	path := writeSecrets(t, `
[anthropic]
setup_token = "sk-global"

[custom]
key = "val"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// No restrictions — all agents see everything
	for _, name := range []string{"alice", "bob", "unknown"} {
		as := s.ForAgent(name)
		v, ok := as.Get("anthropic.setup_token")
		if !ok || v != "sk-global" {
			t.Errorf("%s anthropic.setup_token = %q, ok=%v", name, v, ok)
		}
		v, ok = as.Get("custom.key")
		if !ok || v != "val" {
			t.Errorf("%s custom.key = %q, ok=%v", name, v, ok)
		}
	}
}

func TestHasAgentRestrictions(t *testing.T) {
	// No restrictions
	path := writeSecrets(t, `
[custom]
key = "val"
`)
	s, _ := Load(path)
	if s.HasAgentRestrictions() {
		t.Error("expected false with no restrictions")
	}

	// With allowed_agents
	path = writeSecrets(t, `
[custom]
key = "val"
allowed_agents = ["alice"]
`)
	s, _ = Load(path)
	if !s.HasAgentRestrictions() {
		t.Error("expected true with allowed_agents")
	}

	// With denied_agents
	path = writeSecrets(t, `
[custom]
key = "val"
denied_agents = ["bob"]
`)
	s, _ = Load(path)
	if !s.HasAgentRestrictions() {
		t.Error("expected true with denied_agents")
	}
}

func TestSavePreservesAgentRestrictions(t *testing.T) {
	path := writeSecrets(t, `
[shared]
token = "shared_tok"
allowed_agents = ["alice", "bob"]

[internal]
key = "internal_key"
denied_agents = ["untrusted"]

[open]
val = "open_val"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload and verify
	s2, err := Load(path)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}

	// Values preserved
	v, ok := s2.Get("shared.token")
	if !ok || v != "shared_tok" {
		t.Errorf("shared.token = %q, ok=%v", v, ok)
	}

	// Restrictions preserved — test via ForAgent behavior
	alice := s2.ForAgent("alice")
	charlie := s2.ForAgent("charlie")
	untrusted := s2.ForAgent("untrusted")

	if _, ok := alice.Get("shared.token"); !ok {
		t.Error("alice should see shared.token after roundtrip")
	}
	if _, ok := charlie.Get("shared.token"); ok {
		t.Error("charlie should not see shared.token after roundtrip")
	}
	if _, ok := alice.Get("internal.key"); !ok {
		t.Error("alice should see internal.key after roundtrip")
	}
	if _, ok := untrusted.Get("internal.key"); ok {
		t.Error("untrusted should not see internal.key after roundtrip")
	}
}

func TestAllowedAgentsHostsFiltered(t *testing.T) {
	path := writeSecrets(t, `
[restricted]
token = "restricted_tok"
allowed_hosts = ["api.restricted.com"]
allowed_agents = ["alice"]

[open]
key = "open_key"
allowed_hosts = ["api.open.com"]
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	alice := s.ForAgent("alice")
	bob := s.ForAgent("bob")

	// Alice sees restricted hosts
	hosts := alice.SectionAllowedHosts("restricted")
	if len(hosts) != 1 || hosts[0] != "api.restricted.com" {
		t.Errorf("alice restricted hosts = %v", hosts)
	}

	// Bob does not see restricted hosts
	hosts = bob.SectionAllowedHosts("restricted")
	if len(hosts) != 0 {
		t.Errorf("bob should not see restricted hosts, got %v", hosts)
	}

	// Both see open hosts
	hosts = alice.SectionAllowedHosts("open")
	if len(hosts) != 1 || hosts[0] != "api.open.com" {
		t.Errorf("alice open hosts = %v", hosts)
	}
	hosts = bob.SectionAllowedHosts("open")
	if len(hosts) != 1 || hosts[0] != "api.open.com" {
		t.Errorf("bob open hosts = %v", hosts)
	}
}

// Verifies Load returns an error for non-NotExist read errors (e.g. permission denied).
func TestLoadReadError(t *testing.T) {
	path := writeSecrets(t, `[test]
key = "val"
`)
	os.Chmod(path, 0000)
	t.Cleanup(func() { os.Chmod(path, 0644) })

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unreadable secrets file")
	}
	if !strings.Contains(err.Error(), "read secrets") {
		t.Errorf("error = %q, want 'read secrets'", err)
	}
}

// Verifies Load handles [agents] sub-entries that aren't tables (skips them).
func TestLoadAgentsNonMapValue(t *testing.T) {
	path := writeSecrets(t, `[agents]
not_a_table = "just a string"

[agents.valid.custom]
key = "val"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The non-table value should be silently skipped
	fs := s.ForAgent("valid")
	v, ok := fs.Get("custom.key")
	if !ok || v != "val" {
		t.Errorf("valid custom.key = %q, ok=%v", v, ok)
	}
}

// Verifies Load silently skips unknown value types (e.g. booleans).
func TestLoadUnknownValueType(t *testing.T) {
	path := writeSecrets(t, `[section]
key = "valid"
flag = true
ratio = 3.14
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// String key should be loaded
	v, ok := s.Get("section.key")
	if !ok || v != "valid" {
		t.Errorf("section.key = %q, ok=%v", v, ok)
	}
	// Bool and float should be silently skipped
	_, ok = s.Get("section.flag")
	if ok {
		t.Error("boolean value should be silently skipped")
	}
	_, ok = s.Get("section.ratio")
	if ok {
		t.Error("float value should be silently skipped")
	}
}

// Verifies flattenInto skips non-table values in agent sub-sections.
func TestLoadAgentNonTableSubValue(t *testing.T) {
	// In TOML, [agents.myagent] with scalar values should be skipped
	path := writeSecrets(t, `[agents.myagent]
not_a_section = "scalar"

[agents.myagent.valid]
key = "val"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fs := s.ForAgent("myagent")
	v, ok := fs.Get("valid.key")
	if !ok || v != "val" {
		t.Errorf("myagent valid.key = %q, ok=%v", v, ok)
	}
}

// Verifies flattenInto handles int64 values in agent sub-tables.
func TestLoadAgentIntValue(t *testing.T) {
	path := writeSecrets(t, `[agents.myagent.service]
port = 8080
token = "secret"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fs := s.ForAgent("myagent")
	v, ok := fs.Get("service.port")
	if !ok || v != "8080" {
		t.Errorf("myagent service.port = %q, ok=%v", v, ok)
	}
}

// Verifies AllowedHosts returns nil for names without a dot separator.
func TestAllowedHostsNoDot(t *testing.T) {
	s := &Store{
		allowedHosts: map[string][]string{"section": {"host.com"}},
	}
	hosts := s.AllowedHosts("nodot")
	if hosts != nil {
		t.Errorf("AllowedHosts(nodot) = %v, want nil", hosts)
	}
}

// Verifies CheckHostAllowed returns an error for unparseable URLs.
func TestCheckHostAllowedInvalidURL(t *testing.T) {
	s := &Store{
		allowedHosts: map[string][]string{"section": {"host.com"}},
	}
	err := s.CheckHostAllowed("section.key", "://invalid")
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
	if !strings.Contains(err.Error(), "invalid URL") {
		t.Errorf("error = %q, want 'invalid URL'", err)
	}
}

// Verifies CheckSecurity returns an error message when stat fails with non-NotExist error.
func TestCheckSecurityStatError(t *testing.T) {
	// Create a path where the parent is a file, so stat fails with ENOTDIR
	tmpDir := t.TempDir()
	blocker := filepath.Join(tmpDir, "blocker")
	os.WriteFile(blocker, []byte("file"), 0644)
	badPath := filepath.Join(blocker, "secrets.toml") // can't stat — parent is a file

	s := &Store{
		path:         badPath,
		values:       map[string]string{},
		allowedHosts: map[string][]string{},
		allowedAgents: map[string][]string{},
		deniedAgents:  map[string][]string{},
	}
	warnings := s.CheckSecurity()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "cannot stat") {
		t.Errorf("expected 'cannot stat' warning, got: %v", warnings)
	}
}

// Verifies CheckSecurity returns a warning when the security group doesn't exist.
func TestCheckSecurityGroupNotFound(t *testing.T) {
	orig := securityGroupName
	securityGroupName = "nonexistent-group-for-test"
	defer func() { securityGroupName = orig }()

	path := writeSecrets(t, `[test]
key = "val"
`)
	s, _ := Load(path)
	warnings := s.CheckSecurity()
	foundGroup := false
	for _, w := range warnings {
		if strings.Contains(w, "not found") {
			foundGroup = true
		}
	}
	if !foundGroup {
		t.Errorf("expected 'group not found' warning, got: %v", warnings)
	}
}

// Verifies writeKeyValues handles an empty map correctly (returns early).
func TestSaveEmptySection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.toml")
	s := &Store{
		path:         path,
		values:       map[string]string{},
		allowedHosts: map[string][]string{"orphan": {"host.com"}},
		allowedAgents: map[string][]string{},
		deniedAgents:  map[string][]string{},
	}
	// Save with only allowedHosts (no values for that section) → writeKeyValues gets nil map
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "orphan") {
		t.Errorf("saved content should contain orphan section: %s", data)
	}
}

// Verifies flatKeysToSections skips keys without a dot separator.
func TestFlatKeysToSectionsNoDot(t *testing.T) {
	result := flatKeysToSections(map[string]string{
		"normal.key": "val1",
		"nodot":      "val2",
	})
	if len(result) != 1 {
		t.Errorf("expected 1 section, got %d", len(result))
	}
	if result["normal"]["key"] != "val1" {
		t.Errorf("normal.key = %q", result["normal"]["key"])
	}
}

// Verifies CheckSecurity does not emit a "supplementary groups" warning when
// the process actually belongs to the security group. Dynamically discovers
// a group the process belongs to via Getgroups.
func TestCheckSecurityGroupFound(t *testing.T) {
	gids, err := syscall.Getgroups()
	if err != nil || len(gids) == 0 {
		t.Skip("cannot determine supplementary groups")
	}
	grp, err := user.LookupGroupId(fmt.Sprintf("%d", gids[0]))
	if err != nil {
		t.Skipf("cannot look up gid %d: %v", gids[0], err)
	}

	orig := securityGroupName
	securityGroupName = grp.Name
	defer func() { securityGroupName = orig }()

	path := writeSecrets(t, `[test]
key = "val"
`)
	s, _ := Load(path)
	warnings := s.CheckSecurity()
	for _, w := range warnings {
		if strings.Contains(w, "supplementary groups") {
			t.Errorf("unexpected supplementary groups warning: %s", w)
		}
	}
}

// TestGeneratePassphraseWordCount tests GeneratePassphrase with various word counts
func TestGeneratePassphraseWordCount(t *testing.T) {
	tests := []struct {
		name      string
		wordCount int
	}{
		{"one word", 1},
		{"short", 4},
		{"standard", 5},
		{"long", 8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			phrase, err := GeneratePassphrase(tt.wordCount)
			if err != nil {
				t.Fatalf("GeneratePassphrase(%d): %v", tt.wordCount, err)
			}
			// Count hyphens - should be wordCount - 1
			expectedHyphens := tt.wordCount - 1
			actualHyphens := strings.Count(phrase, "-")
			if actualHyphens != expectedHyphens {
				t.Errorf("GeneratePassphrase(%d) hyphens = %d, want %d", tt.wordCount, actualHyphens, expectedHyphens)
			}
			// Should contain only hyphens and lowercase letters
			for _, c := range phrase {
				if !((c >= 'a' && c <= 'z') || c == '-') {
					t.Errorf("unexpected character %q in passphrase", c)
				}
			}
		})
	}
}

// TestGeneratePassphraseZeroError tests error case for zero words
func TestGeneratePassphraseZeroError(t *testing.T) {
	_, err := GeneratePassphrase(0)
	if err == nil {
		t.Error("GeneratePassphrase(0) should error")
	}
}

// TestAllowedHostsPerSecret tests AllowedHosts with per-secret lookup
func TestAllowedHostsPerSecret(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "secrets.toml")
	content := `
[anthropic]
api_key = "sk-123"
allowed_hosts = ["api.anthropic.com", "api2.anthropic.com"]

[custom]
token = "custom-token"
allowed_hosts = ["api.custom.com"]
`
	os.WriteFile(path, []byte(content), 0600)

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Test AllowedHosts with specific secret
	hosts := s.AllowedHosts("anthropic.api_key")
	if len(hosts) != 2 || hosts[0] != "api.anthropic.com" {
		t.Errorf("AllowedHosts(anthropic.api_key) = %v", hosts)
	}

	// Test with custom section
	hosts = s.AllowedHosts("custom.token")
	if len(hosts) != 1 || hosts[0] != "api.custom.com" {
		t.Errorf("AllowedHosts(custom.token) = %v", hosts)
	}

	// Test with non-existent section
	hosts = s.AllowedHosts("nonexistent.key")
	if len(hosts) != 0 {
		t.Errorf("AllowedHosts(nonexistent.key) = %v, want nil/empty", hosts)
	}
}

// TestCheckHostAllowedSuccess tests CheckHostAllowed with valid host
func TestCheckHostAllowedSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "secrets.toml")
	content := `
[anthropic]
api_key = "sk-123"
allowed_hosts = ["api.anthropic.com"]
`
	os.WriteFile(path, []byte(content), 0600)

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Valid host in allowed list
	err = s.CheckHostAllowed("anthropic.api_key", "https://api.anthropic.com/v1/messages")
	if err != nil {
		t.Errorf("CheckHostAllowed with valid host: %v", err)
	}
}

// TestCheckHostAllowedFailure tests CheckHostAllowed with invalid host
func TestCheckHostAllowedFailure(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "secrets.toml")
	content := `
[anthropic]
api_key = "sk-123"
allowed_hosts = ["api.anthropic.com"]
`
	os.WriteFile(path, []byte(content), 0600)

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Invalid host not in allowed list
	err = s.CheckHostAllowed("anthropic.api_key", "https://evil.com/steal")
	if err == nil {
		t.Error("CheckHostAllowed with invalid host should error")
	}
}

// TestIsBlockedPathDefault tests default blocked paths
func TestIsBlockedPathDefault(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "secrets.toml")
	os.WriteFile(path, []byte(""), 0600)

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Default blocked paths
	if !s.IsBlockedPath("secrets.toml") {
		t.Error("secrets.toml should be blocked by default")
	}
	if !s.IsBlockedPath("/proc/self/environ") {
		t.Error("/proc/self/environ should be blocked by default")
	}

	// Non-blocked path
	if s.IsBlockedPath("/tmp/test.txt") {
		t.Error("/tmp/test.txt should not be blocked")
	}
}

// TestAddAndCheckBlockedPaths tests adding blocked paths
func TestAddAndCheckBlockedPaths(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "secrets.toml")
	os.WriteFile(path, []byte(""), 0600)

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Add custom blocked paths
	s.AddBlockedPaths([]string{"/root/.ssh/id_rsa", "/etc/passwd"})

	if !s.IsBlockedPath("/root/.ssh/id_rsa") {
		t.Error("/root/.ssh/id_rsa should be blocked after AddBlockedPaths")
	}
	if !s.IsBlockedPath("/etc/passwd") {
		t.Error("/etc/passwd should be blocked after AddBlockedPaths")
	}
}
