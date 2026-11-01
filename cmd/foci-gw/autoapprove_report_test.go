package main

import (
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/execguard"
	"foci/internal/execguard/execguardtest"
	"foci/internal/log"
)

// delegatedAgent is an agent the built-in groups are live for: a delegated
// backend that consults foci's allowlist and still asks for approval. The
// report mirrors buildAutoApproveRules' serving condition (#1897), so every
// toggle and WARN fixture needs one.
func delegatedAgent(id string) config.AgentConfig {
	return config.AgentConfig{ID: id, Backend: "claude-code"}
}

// TestWarnSubstitutableBuiltins_LogsGroupCommandReasonPath proves the
// startup half of the built-in report (#1897): one WARN per built-in group
// command the match-time veto will refuse, carrying the group, the command,
// the reason and the path execguard gave, and saying the command falls
// through to an approval prompt.
func TestWarnSubstitutableBuiltins_LogsGroupCommandReasonPath(t *testing.T) {
	t.Parallel()
	c := captureLog(t, log.WARN)
	dir := t.TempDir()
	env := execguardtest.Env([]string{dir}, []string{dir + "/mds"}, []string{dir + "/mds"})
	// One live agent is enough: read-only defaults on, safe-write defaults off.
	cfg := &config.Config{Agents: []config.AgentConfig{delegatedAgent("main")}}

	warnSubstitutableBuiltins(cfg, env)

	// Filtered to this test's own command, not just the group line: the tap
	// sees every parallel test's WARNs, and reportOperatorEnv's test warns
	// about another built-in read-only command.
	lines := c.Lines("built-in read-only group", `command "mds"`)
	if len(lines) != 1 {
		t.Fatalf("got %d built-in read-only WARN(s), want 1:\n%s", len(lines), c.String())
	}
	if lines[0].Level != log.WARN || lines[0].Component != "config" {
		t.Errorf("line = %s, want a config-component WARN", lines[0].String())
	}
	line := lines[0].Msg
	for _, want := range []string{`command "mds"`, "approval prompt"} {
		if !strings.Contains(line, want) {
			t.Errorf("WARN lacks %q: %s", want, line)
		}
	}
	if _, path, reason := execguard.Substitutable("mds", env); !strings.Contains(line, reason) || !strings.Contains(line, path) {
		t.Errorf("WARN must carry execguard's own reason and path, got %q (want reason %q, path %q)", line, reason, path)
	}
}

// TestWarnSubstitutableBuiltins_NoLiveAgentNoWarn pins the other half of the
// serving condition: an agent whose shells never consult the built-in groups
// produces no WARN, because nothing it runs can stop auto-approving. An API
// agent never gets the groups (buildAutoApproveRules runs for delegated
// backends only), and a skip_permissions agent's backend runs everything
// unattended — for either, "falls through to an approval prompt" would be
// false.
func TestWarnSubstitutableBuiltins_NoLiveAgentNoWarn(t *testing.T) {
	t.Parallel()
	c := captureLog(t, log.WARN)
	dir := t.TempDir()
	env := execguardtest.Env([]string{dir}, []string{dir + "/mds"}, []string{dir + "/mds"})
	yes := true
	for name, cfg := range map[string]*config.Config{
		"API agent only": {Agents: []config.AgentConfig{{ID: "main"}}},
		"skip_permissions agent": {Agents: []config.AgentConfig{{
			ID:            "main",
			Backend:       "claude-code",
			BackendConfig: config.BackendConfig{SkipPermissions: &yes},
		}}},
	} {
		warnSubstitutableBuiltins(cfg, env)
		// Filtered to this test's own temp dir: the tap is process-wide, so
		// an unfiltered negative check also sees the built-in WARNs parallel
		// tests emit and fails intermittently. Every WARN this call could
		// wrongly log carries the path of this test's mds, under dir.
		if lines := c.Lines("built-in", dir+"/"); len(lines) != 0 {
			t.Errorf("%s: got %d built-in WARN(s), want none — no shell of these agents consults the groups:\n%s", name, len(lines), c.String())
		}
	}
}
