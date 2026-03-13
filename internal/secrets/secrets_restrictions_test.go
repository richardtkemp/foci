package secrets

import (
	"strings"
	"testing"
)

func TestAllowedAgentsWhitelist(t *testing.T) {
	// TestAllowedAgentsWhitelist proves that allowed_agents acts as an allowlist: only
	// agents explicitly named can read the restricted secret, while unrestricted sections
	// remain accessible to everyone.
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

	v, ok := alice.Get("shared_api.token")
	if !ok || v != "shared_token" {
		t.Errorf("alice shared_api.token = %q, ok=%v", v, ok)
	}
	v, ok = bob.Get("shared_api.token")
	if !ok || v != "shared_token" {
		t.Errorf("bob shared_api.token = %q, ok=%v", v, ok)
	}

	_, ok = charlie.Get("shared_api.token")
	if ok {
		t.Error("charlie should not see shared_api.token")
	}

	for _, name := range []string{"alice", "bob", "charlie"} {
		as := s.ForAgent(name)
		v, ok := as.Get("open.key")
		if !ok || v != "open_key" {
			t.Errorf("%s open.key = %q, ok=%v", name, v, ok)
		}
	}
}

func TestDeniedAgentsBlacklist(t *testing.T) {
	// TestDeniedAgentsBlacklist proves that denied_agents acts as a denylist: the denied
	// agent cannot read the restricted secret while all others can, and unrestricted
	// sections remain visible to everyone including the denied agent.
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

	v, ok := trusted.Get("internal.token")
	if !ok || v != "internal_token" {
		t.Errorf("trusted internal.token = %q, ok=%v", v, ok)
	}

	_, ok = untrusted.Get("internal.token")
	if ok {
		t.Error("untrusted should not see internal.token")
	}

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
	// TestBothAllowedAndDeniedError proves that specifying both allowed_agents and
	// denied_agents on the same section is rejected at load time with a clear error.
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
	// TestAgentOverrideSurvivesDeny proves that an agent's own per-agent section can
	// contain keys from a globally denied section — the override is visible, but the
	// denied global key in the same section is still hidden.
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
	v, ok := alice.Get("custom.agent_key")
	if !ok || v != "alice_val" {
		t.Errorf("alice custom.agent_key = %q, ok=%v", v, ok)
	}

	_, ok = alice.Get("custom.global_key")
	if ok {
		t.Error("alice should not see denied global_key")
	}
}

func TestNoRestrictionsDefault(t *testing.T) {
	// TestNoRestrictionsDefault proves that a section without any agent restriction fields
	// is accessible by every agent, including those with no dedicated per-agent section.
	path := writeSecrets(t, `
[unrestricted]
token = "token_val"

[agents.charlie.other]
key = "val"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	for _, agent := range []string{"charlie", "diana", "nobody"} {
		as := s.ForAgent(agent)
		v, ok := as.Get("unrestricted.token")
		if !ok || v != "token_val" {
			t.Errorf("%s unrestricted.token = %q, ok=%v", agent, v, ok)
		}
	}
}

func TestHasAgentRestrictions(t *testing.T) {
	// TestHasAgentRestrictions proves that HasAgentRestrictions returns false when no
	// sections carry any restriction fields, and true when any section uses either
	// allowed_agents or denied_agents.
	pathNoRestrict := writeSecrets(t, `
[open]
key = "val"
`)
	s1, _ := Load(pathNoRestrict)
	if s1.HasAgentRestrictions() {
		t.Error("store with no restrictions should return false")
	}

	pathWithRestrict := writeSecrets(t, `
[restricted_allow]
token = "val"
allowed_agents = ["alice"]
`)
	s2, _ := Load(pathWithRestrict)
	if !s2.HasAgentRestrictions() {
		t.Error("store with allowed_agents should return true")
	}

	pathDenyRestrict := writeSecrets(t, `
[restricted_deny]
key = "val"
denied_agents = ["bob"]
`)
	s3, _ := Load(pathDenyRestrict)
	if !s3.HasAgentRestrictions() {
		t.Error("store with denied_agents should return true")
	}
}

func TestSavePreservesAgentRestrictions(t *testing.T) {
	// TestSavePreservesAgentRestrictions proves that both allowed_agents and denied_agents
	// fields survive a save/load roundtrip: allowed agents can still access their secrets
	// and denied agents remain blocked after reload.
	path := writeSecrets(t, `
[restricted_allow]
token = "token_val"
allowed_agents = ["alice", "bob"]

[restricted_deny]
key = "key_val"
denied_agents = ["untrusted"]

[unrestricted]
generic = "generic_val"
`)
	s, _ := Load(path)

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s2, _ := Load(path)

	alice := s2.ForAgent("alice")
	v, ok := alice.Get("restricted_allow.token")
	if !ok || v != "token_val" {
		t.Errorf("alice restricted_allow.token = %q, ok=%v", v, ok)
	}

	untrusted := s2.ForAgent("untrusted")
	_, ok = untrusted.Get("restricted_deny.key")
	if ok {
		t.Error("untrusted should not see restricted_deny.key")
	}
}

func TestAllowedAgentsHostsFiltered(t *testing.T) {
	// TestAllowedAgentsHostsFiltered proves that AllowedHosts respects agent restrictions:
	// an agent in the allowlist sees the hosts, while an agent not in the allowlist gets
	// nil as if the secret doesn't exist for them.
	path := writeSecrets(t, `
[myapi]
token = "sk-test"
allowed_agents = ["alice"]
allowed_hosts = ["api.example.com"]
`)
	s, _ := Load(path)

	alice := s.ForAgent("alice")
	hosts := alice.AllowedHosts("myapi.token")
	if len(hosts) != 1 || hosts[0] != "api.example.com" {
		t.Errorf("alice hosts = %v", hosts)
	}

	bob := s.ForAgent("bob")
	hosts = bob.AllowedHosts("myapi.token")
	if hosts != nil {
		t.Errorf("bob should not see hosts (denied by allowed_agents): %v", hosts)
	}
}
