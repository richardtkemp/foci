package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A pure claude-code (delegated-backend) deployment routes ALL LLM work — agent
// turns, compaction, summaries, memory — through the backend, never the model
// groups. So first-run writes no [groups]/[models.default]/[endpoints]. These
// tests pin the two consequences that matter and that previously misbehaved:
//
//  1. Loading such a config derives NO anthropic endpoint, so foci never looks
//     for anthropic credentials (no spurious "missing secret" warning).
//  2. No model group resolves, so nothing — e.g. the periodic runner's
//     chat-call resolution — tries to build an API client (the source of the
//     spurious "no Anthropic credentials — run: foci auth" startup error).
//
// Load() also calls Validate(), so a clean load additionally proves a
// groupless config validates (the [groups] powerful requirement is relaxed when
// no API-backed agent is present).
const claudeCodeOnlyTOML = `
[[agents]]
id = "cctest"
backend = "claude-code"

[agents.backend_config]
model = "sonnet"
`

func loadClaudeCodeOnly(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	if err := os.WriteFile(path, []byte(claudeCodeOnlyTOML), 0644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() of a claude-code-only config failed (it should validate cleanly with no groups): %v", err)
	}
	return cfg
}

func TestClaudeCodeOnlyConfig_DerivesNoAnthropicEndpoint(t *testing.T) {
	cfg := loadClaudeCodeOnly(t)
	if _, ok := cfg.Endpoints["anthropic"]; ok {
		t.Error("anthropic endpoint must NOT be derived for a claude-code-only config — it makes foci look for anthropic credentials it will never use")
	}
	// A backend-only config references no model developers, so NO endpoints of
	// any kind should be auto-created.
	if n := len(cfg.Endpoints); n != 0 {
		t.Errorf("expected no derived endpoints for a claude-code-only config, got %d: %v", n, cfg.Endpoints)
	}
}

func TestClaudeCodeOnlyConfig_NoModelGroupResolves(t *testing.T) {
	cfg := loadClaudeCodeOnly(t)
	if n := len(cfg.Groups.Groups); n != 0 {
		t.Errorf("expected no model groups for a claude-code-only config, got %d: %v", n, cfg.Groups.Groups)
	}
	// The chat call site is what the periodic runner resolves; with no groups it
	// must come back nil so no API client is ever built.
	gr := NewGroupResolver(cfg.Groups, cfg.Models, cfg.HasAPIAgent())
	if resolved := gr.ResolveCall(CallChat); resolved != nil {
		t.Errorf("ResolveCall(CallChat) must be nil when no groups are defined; got %+v", resolved)
	}
}
