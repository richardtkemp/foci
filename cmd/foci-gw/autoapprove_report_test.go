package main

import (
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/execguard"
	"foci/internal/log"
)

// TestWarnSubstitutableBuiltins_LogsGroupCommandReasonPath proves the
// startup half of the built-in report (#1897): one WARN per built-in group
// command the match-time veto will refuse, carrying the group, the command,
// the reason and the path execguard gave, and saying the command falls
// through to an approval prompt.
func TestWarnSubstitutableBuiltins_LogsGroupCommandReasonPath(t *testing.T) {
	t.Parallel()
	c := captureLog(t, log.WARN)
	dir := t.TempDir()
	env := guardEnvFor([]string{dir}, []string{dir + "/mds"}, []string{dir + "/mds"})
	// One agent is enough: read-only defaults on, safe-write defaults off.
	cfg := &config.Config{Agents: []config.AgentConfig{{ID: "main"}}}

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

// TestAutoApproveGroupToggles_PerAgentTrueUnderGlobalFalse pins the group
// gating to config.Resolve's cascade: a per-agent true under a global false
// enables the group's report (one agent running the group is enough for the
// veto to hit), a group disabled everywhere is not reported, and read-only
// defaults on with no overrides.
func TestAutoApproveGroupToggles_PerAgentTrueUnderGlobalFalse(t *testing.T) {
	t.Parallel()
	no, yes := false, true
	perAgentOn := &config.Config{
		Permissions: config.PermissionsConfig{AutoApproveCommonReadonly: &no, AutoApproveCommonSafeWrite: &no},
		Agents: []config.AgentConfig{
			{ID: "a", Permissions: config.PermissionsConfig{AutoApproveCommonReadonly: &yes}},
			{ID: "b"},
		},
	}
	if ro, sw := autoApproveGroupToggles(perAgentOn); !ro || sw {
		t.Errorf("toggles = (%v, %v), want (true, false): a per-agent true under a global false must count", ro, sw)
	}

	allOff := &config.Config{
		Permissions: config.PermissionsConfig{AutoApproveCommonReadonly: &no, AutoApproveCommonSafeWrite: &no},
		Agents:      []config.AgentConfig{{ID: "a"}, {ID: "b"}},
	}
	if ro, sw := autoApproveGroupToggles(allOff); ro || sw {
		t.Errorf("toggles = (%v, %v), want (false, false): a group disabled everywhere is not reported", ro, sw)
	}

	defaults := &config.Config{Agents: []config.AgentConfig{{ID: "a"}}}
	if ro, sw := autoApproveGroupToggles(defaults); !ro || sw {
		t.Errorf("toggles = (%v, %v), want (true, false): read-only defaults on, safe-write defaults off", ro, sw)
	}
}
