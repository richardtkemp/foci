package main

import (
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/execguard"
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
	env := guardEnvFor([]string{dir}, []string{dir + "/mds"}, []string{dir + "/mds"})
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
	env := guardEnvFor([]string{dir}, []string{dir + "/mds"}, []string{dir + "/mds"})
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
		if lines := c.Lines("built-in"); len(lines) != 0 {
			t.Errorf("%s: got %d built-in WARN(s), want none — no shell of these agents consults the groups:\n%s", name, len(lines), c.String())
		}
	}
}

// TestAutoApproveGroupToggles_PerAgentTrueUnderGlobalFalse pins the group
// gating to config.Resolve's cascade over the agents the groups are live
// for: a per-agent true under a global false enables the group's report (one
// agent running the group is enough for the veto to hit), a group disabled
// everywhere is not reported, and read-only defaults on with no overrides.
func TestAutoApproveGroupToggles_PerAgentTrueUnderGlobalFalse(t *testing.T) {
	t.Parallel()
	no, yes := false, true
	perAgentOn := &config.Config{
		Permissions: config.PermissionsConfig{AutoApproveCommonReadonly: &no, AutoApproveCommonSafeWrite: &no},
		Agents: []config.AgentConfig{
			{ID: "a", Backend: "claude-code", Permissions: config.PermissionsConfig{AutoApproveCommonReadonly: &yes}},
			delegatedAgent("b"),
		},
	}
	if ro, sw := autoApproveGroupToggles(perAgentOn); !ro || sw {
		t.Errorf("toggles = (%v, %v), want (true, false): a per-agent true under a global false must count", ro, sw)
	}

	allOff := &config.Config{
		Permissions: config.PermissionsConfig{AutoApproveCommonReadonly: &no, AutoApproveCommonSafeWrite: &no},
		Agents:      []config.AgentConfig{delegatedAgent("a"), delegatedAgent("b")},
	}
	if ro, sw := autoApproveGroupToggles(allOff); ro || sw {
		t.Errorf("toggles = (%v, %v), want (false, false): a group disabled everywhere is not reported", ro, sw)
	}

	defaults := &config.Config{Agents: []config.AgentConfig{delegatedAgent("a")}}
	if ro, sw := autoApproveGroupToggles(defaults); !ro || sw {
		t.Errorf("toggles = (%v, %v), want (true, false): read-only defaults on, safe-write defaults off", ro, sw)
	}
}

// TestAutoApproveGroupToggles_OnlyLiveAgentsCount pins the serving condition
// around the cascade: a toggle an agent's shells never act on must not put
// its group in the report. An API agent never receives the groups, and a
// skip_permissions agent's backend never raises the prompt the groups would
// settle — for both, veto lines naming the group would be false.
func TestAutoApproveGroupToggles_OnlyLiveAgentsCount(t *testing.T) {
	t.Parallel()
	no, yes := false, true

	apiOnly := &config.Config{Agents: []config.AgentConfig{{ID: "main"}}}
	if ro, sw := autoApproveGroupToggles(apiOnly); ro || sw {
		t.Errorf("toggles = (%v, %v), want (false, false): an API agent never gets the built-in groups, even with read-only defaulting on", ro, sw)
	}

	// The API agent turns read-only on under a global off: still inert.
	apiTurnsOn := &config.Config{
		Permissions: config.PermissionsConfig{AutoApproveCommonReadonly: &no},
		Agents: []config.AgentConfig{
			{ID: "api", Permissions: config.PermissionsConfig{AutoApproveCommonReadonly: &yes}},
			{ID: "cc", Backend: "claude-code", Permissions: config.PermissionsConfig{AutoApproveCommonReadonly: &no}},
		},
	}
	if ro, sw := autoApproveGroupToggles(apiTurnsOn); ro || sw {
		t.Errorf("toggles = (%v, %v), want (false, false): only the API agent enabled the group", ro, sw)
	}

	skipOnly := &config.Config{Agents: []config.AgentConfig{{
		ID:            "unattended",
		Backend:       "claude-code",
		BackendConfig: config.BackendConfig{SkipPermissions: &yes},
		Permissions:   config.PermissionsConfig{AutoApproveCommonReadonly: &no, AutoApproveCommonSafeWrite: &yes},
	}}}
	if ro, sw := autoApproveGroupToggles(skipOnly); ro || sw {
		t.Errorf("toggles = (%v, %v), want (false, false): a skip_permissions agent never prompts, so nothing there stops auto-approving", ro, sw)
	}

	// Control: the same skip_permissions agent beside one live agent that
	// turns the group on — the live agent's toggle is what counts.
	skipPlusLive := &config.Config{
		Permissions: config.PermissionsConfig{AutoApproveCommonReadonly: &no},
		Agents: []config.AgentConfig{
			{ID: "unattended", Backend: "claude-code", BackendConfig: config.BackendConfig{SkipPermissions: &yes}},
			{ID: "cc", Backend: "claude-code", Permissions: config.PermissionsConfig{AutoApproveCommonSafeWrite: &yes}},
		},
	}
	if ro, sw := autoApproveGroupToggles(skipPlusLive); ro || !sw {
		t.Errorf("toggles = (%v, %v), want (false, true): the live agent's safe-write toggle must count", ro, sw)
	}
}

// TestAutoApproveGroupToggles_AllowlistCapabilityRequired names the last leg
// of the serving condition: a delegated backend that does not declare
// CapCommandApprovalAllowlist never matches a rule against a permission
// request, so no command of its agents can stop auto-approving either. The
// fake Spec declares every capability NotApplicable on purpose — the
// capability table is the contract, not the backend's identity.
func TestAutoApproveGroupToggles_AllowlistCapabilityRequired(t *testing.T) {
	t.Parallel()
	const name = "foci-gw-test-no-allowlist"
	caps := make(map[delegator.Capability]delegator.Support)
	for _, c := range delegator.AllCapabilities() {
		caps[c] = delegator.NotApplicable("test backend")
	}
	delegator.Register(delegator.Spec{
		Name:         name,
		DisplayName:  "test backend without the allowlist",
		New:          func(map[string]any) (delegator.Delegator, error) { return nil, nil },
		LedgerKey:    "test",
		ModelcapsKey: "test",
		Caps:         caps,
	})

	cfg := &config.Config{Agents: []config.AgentConfig{{ID: "x", Backend: name}}}
	if ro, sw := autoApproveGroupToggles(cfg); ro || sw {
		t.Errorf("toggles = (%v, %v), want (false, false): a backend that does not consult foci's allowlist never matches a rule", ro, sw)
	}
}
