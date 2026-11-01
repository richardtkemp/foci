package configcheck

import (
	"testing"

	"foci/internal/config"
	"foci/internal/delegator"
	_ "foci/internal/delegator/all" // register claude-code for the toggle fixtures
)

// delegatedAgent is an agent the built-in groups are live for: a delegated
// backend that consults foci's allowlist and still asks for approval. The
// report mirrors buildAutoApproveRules' serving condition (#1897), so every
// toggle fixture needs one.
func delegatedAgent(id string) config.AgentConfig {
	return config.AgentConfig{ID: id, Backend: "claude-code"}
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
