package secrets

import (
	"strings"
	"testing"
)

// TestLoadPerAgentSecrets verifies loading and accessing per-agent overrides.
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

	v, ok := s.Get("anthropic.setup_token")
	if !ok || v != "sk-global" {
		t.Errorf("global anthropic.setup_token = %q, ok=%v", v, ok)
	}

	fs := s.ForAgent("fotini")
	v, ok = fs.Get("custom.github_token")
	if !ok || v != "ghp_fotini" {
		t.Errorf("fotini custom.github_token = %q, ok=%v", v, ok)
	}

	v, ok = fs.Get("custom.deploy_key")
	if !ok || v != "dk_fotini" {
		t.Errorf("fotini custom.deploy_key = %q, ok=%v", v, ok)
	}

	v, ok = fs.Get("anthropic.setup_token")
	if !ok || v != "sk-global" {
		t.Errorf("fotini anthropic.setup_token = %q, ok=%v", v, ok)
	}

	hosts := fs.AllowedHosts("myapi.token")
	if len(hosts) != 1 || hosts[0] != "api.fotini.com" {
		t.Errorf("fotini AllowedHosts(myapi.token) = %v", hosts)
	}
}

// TestForAgentOverridesGlobal verifies agent overrides win over global.
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

	v, _ = s.Get("custom.api_key")
	if v != "global_key" {
		t.Errorf("expected global_key on root, got %q", v)
	}
}

// TestForAgentFallbackToGlobal verifies missing agent keys fall back to global.
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

	v, _ := bs.Get("custom.key_a")
	if v != "beta_a" {
		t.Errorf("expected beta_a, got %q", v)
	}
	v, _ = bs.Get("custom.key_b")
	if v != "val_b" {
		t.Errorf("expected val_b, got %q", v)
	}
	v, _ = bs.Get("anthropic.setup_token")
	if v != "sk-global" {
		t.Errorf("expected sk-global, got %q", v)
	}
}

// TestForAgentIsolation verifies agents can't see each other's secrets.
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

	v, ok := alice.Get("custom.private")
	if !ok || v != "alice_secret" {
		t.Errorf("alice custom.private = %q, ok=%v", v, ok)
	}
	v, ok = bob.Get("custom.private")
	if !ok || v != "bob_secret" {
		t.Errorf("bob custom.private = %q, ok=%v", v, ok)
	}

	v, _ = alice.Get("custom.shared")
	if v != "global" {
		t.Errorf("alice custom.shared = %q", v)
	}
	v, _ = bob.Get("custom.shared")
	if v != "global" {
		t.Errorf("bob custom.shared = %q", v)
	}
}

// TestForAgentNames verifies Names() includes both global and agent secrets.
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

// TestForAgentResolve verifies template resolution uses agent secrets.
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

	got, err = s.Resolve("Bearer {{secret:custom.token}}")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "Bearer global_tok" {
		t.Errorf("Resolve on root = %q, want %q", got, "Bearer global_tok")
	}
}

// TestForAgentRedact verifies redaction includes all agent and global secrets.
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

// TestForAgentNoSection verifies agent without section gets global secrets.
func TestForAgentNoSection(t *testing.T) {
	path := writeSecrets(t, `
[anthropic]
setup_token = "sk-global"

[custom]
key = "val"
`)
	s, _ := Load(path)
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

// TestLoadPerAgentBackwardCompat verifies old format without agents still works.
func TestLoadPerAgentBackwardCompat(t *testing.T) {
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

	fs := s.ForAgent("anyagent")
	v, ok = fs.Get("anthropic.setup_token")
	if !ok || v != "sk-ant-test" {
		t.Errorf("ForAgent anthropic.setup_token = %q, ok=%v", v, ok)
	}
}

// TestSavePreservesAgentSections verifies agent sections survive save/load.
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

	s2, err := Load(path)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}

	v, ok := s2.Get("anthropic.setup_token")
	if !ok || v != "sk-global" {
		t.Errorf("anthropic.setup_token = %q, ok=%v", v, ok)
	}

	fs := s2.ForAgent("fotini")
	v, ok = fs.Get("custom.api_key")
	if !ok || v != "fotini_key" {
		t.Errorf("fotini custom.api_key = %q, ok=%v", v, ok)
	}

	hosts := fs.AllowedHosts("custom.api_key")
	if len(hosts) != 1 || hosts[0] != "api.fotini.com" {
		t.Errorf("fotini AllowedHosts = %v", hosts)
	}
}
