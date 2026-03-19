package config

import "testing"

// TestLookupValueGlobalSection verifies that LookupValue resolves a top-level
// config field (sessions.compaction_threshold) to its effective value, which
// includes defaults applied by Load.
func TestLookupValueGlobalSection(t *testing.T) {
	cfg := &Config{
		Sessions: SessionsConfig{CompactionThreshold: 0.8},
	}
	agent := AgentConfig{}

	got := LookupValue(cfg, agent, "sessions", "compaction_threshold")
	if got != "0.8" {
		t.Errorf("LookupValue(sessions, compaction_threshold) = %q, want %q", got, "0.8")
	}
}

// TestLookupValueAgentSection verifies that LookupValue resolves an agent-level
// field using the AgentConfig, not the global Config.
func TestLookupValueAgentSection(t *testing.T) {
	cfg := &Config{}
	agent := AgentConfig{MaxToolLoops: 30}

	got := LookupValue(cfg, agent, "agent", "max_tool_loops")
	if got != "30" {
		t.Errorf("LookupValue(agent, max_tool_loops) = %q, want %q", got, "30")
	}
}

// TestLookupValueDottedKey verifies that LookupValue resolves a dotted key
// (e.g. "keepalive.enabled") by walking nested structs via TOML tags.
func TestLookupValueDottedKey(t *testing.T) {
	cfg := &Config{}
	agent := AgentConfig{
		Keepalive: KeepaliveConfig{Enabled: true, Interval: "5m"},
	}

	got := LookupValue(cfg, agent, "agent", "keepalive.enabled")
	if got != "true" {
		t.Errorf("LookupValue(agent, keepalive.enabled) = %q, want %q", got, "true")
	}

	got = LookupValue(cfg, agent, "agent", "keepalive.interval")
	if got != "5m" {
		t.Errorf("LookupValue(agent, keepalive.interval) = %q, want %q", got, "5m")
	}
}

// TestLookupValueUnknown verifies that LookupValue returns "" for
// nonexistent sections and keys rather than panicking.
func TestLookupValueUnknown(t *testing.T) {
	cfg := &Config{}
	agent := AgentConfig{}

	if got := LookupValue(cfg, agent, "nonexistent", "key"); got != "" {
		t.Errorf("unknown section returned %q", got)
	}
	if got := LookupValue(cfg, agent, "sessions", "nonexistent_key"); got != "" {
		t.Errorf("unknown key returned %q", got)
	}
}

// TestLookupValueBool verifies that boolean fields display as "true"/"false".
func TestLookupValueBool(t *testing.T) {
	cfg := &Config{
		Keepalive: KeepaliveConfig{Enabled: true},
	}
	agent := AgentConfig{}

	got := LookupValue(cfg, agent, "keepalive", "enabled")
	if got != "true" {
		t.Errorf("LookupValue(keepalive, enabled) = %q, want %q", got, "true")
	}
}
