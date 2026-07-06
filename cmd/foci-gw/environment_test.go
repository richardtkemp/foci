package main

import (
	"strings"
	"testing"

	"foci/internal/config"
)

func TestBuildEnvironmentDelegated_SkipPermissionsOmitsApproval(t *testing.T) {
	base := config.AgentConfig{ID: "x", Workspace: "/tmp/x", Backend: "claude-code"}
	cfg := &config.Config{Logging: config.LoggingConfig{EventFile: "/tmp/foci.log"}}

	normal := buildEnvironmentDelegated(base, "/tmp/foci.toml", cfg, config.Resolve(cfg, base), 0, nil, nil, nil)
	if !strings.Contains(normal, "## Command Approval") {
		t.Fatal("expected Command Approval section for a normal claude-code agent")
	}

	skip := base
	skip.BackendConfig = map[string]any{"skip_permissions": true}
	skipped := buildEnvironmentDelegated(skip, "/tmp/foci.toml", cfg, config.Resolve(cfg, skip), 0, nil, nil, nil)
	if strings.Contains(skipped, "## Command Approval") {
		t.Error("skip_permissions should omit the Command Approval section (everything is permitted)")
	}
}

func TestWriteAPIConfig(t *testing.T) {
	acfg := config.AgentConfig{BlockedPaths: []config.BlockedPath{{Path: "/etc"}}}
	cfg := &config.Config{}
	cfg.Tools.ExecDefaultTimeout = 30
	rc := &config.ResolvedAgentConfig{}
	rc.Loop.MaxToolLoops = 25
	rc.Summary.MaxResultChars = 15000
	rc.Summary.AutoSummarise = true
	rc.Tools.MaxFileReadBytes = 52428800
	rc.Tools.MaxConcurrentSpawns = 3
	rc.Tools.ExecAutoBackground = 10

	var b strings.Builder
	writeAPIConfig(&b, acfg, cfg, rc)
	out := b.String()
	for _, want := range []string{
		"## Tool & Loop Limits",
		"up to 25 tool iterations",
		"over 15000 chars",
		"over 50 MB",
		"up to 3 concurrent spawn",
		"30s timeout",
		"after 10s auto-background",
		"(write/edit refused): `/etc`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("API config block missing %q\n---\n%s", want, out)
		}
	}

	var empty strings.Builder
	writeAPIConfig(&empty, config.AgentConfig{}, &config.Config{}, &config.ResolvedAgentConfig{})
	if empty.Len() != 0 {
		t.Errorf("empty config should emit no section, got:\n%s", empty.String())
	}
}

func TestWriteMemorySearch(t *testing.T) {
	mk := func(backend string) (config.AgentConfig, *config.ResolvedAgentConfig) {
		acfg := config.AgentConfig{}
		acfg.Memory.Sources = []config.MemorySource{{Name: "canonical", Dir: "/home/foci/clutch/memory", Weight: 1.0}}
		rc := &config.ResolvedAgentConfig{}
		rc.MemorySearch.SearchBackend = backend
		rc.MemorySearch.SearchLimit = 20
		rc.MemorySearch.ConversationWeight = 0.1
		return acfg, rc
	}

	var bl strings.Builder
	acfg, rc := mk("bleve")
	writeMemorySearch(&bl, acfg, rc)
	for _, want := range []string{"## Memory & Search", "**bleve**", "stemmed", "NOT conversation history", "canonical", "up to 20 results"} {
		if !strings.Contains(strings.ToLower(bl.String()), strings.ToLower(want)) {
			t.Errorf("bleve block missing %q\n---\n%s", want, bl.String())
		}
	}

	var f5 strings.Builder
	acfg, rc = mk("fts5")
	writeMemorySearch(&f5, acfg, rc)
	if !strings.Contains(f5.String(), "and conversation history") {
		t.Errorf("fts5 block should note conversation history is indexed\n---\n%s", f5.String())
	}

	// No backend → no section.
	var none strings.Builder
	writeMemorySearch(&none, config.AgentConfig{}, &config.ResolvedAgentConfig{})
	if none.Len() != 0 {
		t.Errorf("no backend should emit nothing, got:\n%s", none.String())
	}
}

func TestWriteCommandApproval(t *testing.T) {
	var b strings.Builder
	rc := &config.ResolvedAgentConfig{}
	rc.Permissions.AutoApproveCommonReadonly = true
	rc.Permissions.AutoApproveCommonSafeWrite = false
	rc.Permissions.AutoApproveRules = []string{"Bash:gh search", "Bash:git -C /home/rich/git/foci"}
	writeCommandApproval(&b, rc, "Read(/tmp/**), Write(/tmp/**)")
	out := b.String()

	for _, want := range []string{
		"## Command Approval",
		"**CC pre-approved** (auto-run, no prompt — not a restriction): Read(/tmp/**), Write(/tmp/**)", // the CC --allowedTools layer
		"every `foci_*` shell function is always auto-approved",
		"**read-only** (on):",
		"sqlite3 -readonly",           // a rendered read-only rule (Bash: stripped)
		"**safe-write** (off",         // disabled state surfaced
		"**configured for this agent**: gh search, git -C /home/rich/git/foci", // Bash: stripped
	} {
		if !strings.Contains(out, want) {
			t.Errorf("command-approval block missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "Bash:") {
		t.Errorf("Bash: prefix should be stripped for prose readability\n---\n%s", out)
	}
}

func TestWriteBackend(t *testing.T) {
	for _, tt := range []struct {
		backend string
		want    string
		absent  string
	}{
		{"claude-code", "Claude Code", "opencode"},
		{"opencode", "opencode", "Claude Code"}, // regression: opencode agents were mislabelled as Claude Code
		{"api", "native API loop", "Claude Code"},
		{"", "native API loop", ""}, // empty backend → api
	} {
		var b strings.Builder
		writeBackend(&b, tt.backend, nil)
		out := b.String()
		if !strings.Contains(out, "## Backend") {
			t.Errorf("backend=%q: missing Backend section", tt.backend)
		}
		if !strings.Contains(out, tt.want) {
			t.Errorf("backend=%q: want %q in\n%s", tt.backend, tt.want, out)
		}
		if tt.absent != "" && strings.Contains(out, tt.absent) {
			t.Errorf("backend=%q: should not contain %q in\n%s", tt.backend, tt.absent, out)
		}
	}
}

func TestWriteBackend_UnknownYieldsNoSection(t *testing.T) {
	var b strings.Builder
	writeBackend(&b, "no-such-backend", nil)
	if b.Len() != 0 {
		t.Errorf("unknown backend with no file should emit nothing, got:\n%s", b.String())
	}
}

func TestWriteCommandApproval_ReadonlyDisabled(t *testing.T) {
	var b strings.Builder
	rc := &config.ResolvedAgentConfig{}
	rc.Permissions.AutoApproveCommonReadonly = false
	writeCommandApproval(&b, rc, "")
	out := b.String()
	if strings.Contains(out, "**read-only** (on)") {
		t.Error("read-only line should be absent when the allowlist is disabled")
	}
	if strings.Contains(out, "CC pre-approved") {
		t.Error("CC pre-approved line should be absent when no --allowedTools are configured")
	}
}
