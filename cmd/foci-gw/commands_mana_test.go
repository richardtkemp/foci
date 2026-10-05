package main

import (
	"testing"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/config"
)

// TestRegisterAgentCommands_ManaOnlyForClaudeCode pins #1543: /mana (and its
// /usage alias) reports the operator's Claude Code plan usage, so it must only
// exist for Claude Code backends. An opencode agent (arnix) used to see it and
// get Claude's numbers back.
func TestRegisterAgentCommands_ManaOnlyForClaudeCode(t *testing.T) {
	cases := []struct {
		backend string
		want    bool
	}{
		{"claude-code", true},
		{"opencode", false},
		{"codex", false},
		{"", false}, // API-mode agent
	}
	for _, tc := range cases {
		t.Run("backend="+tc.backend, func(t *testing.T) {
			resolved := &config.ResolvedAgentConfig{}
			p := cmdRegParams{
				ag:           &agent.Agent{},
				acfg:         config.AgentConfig{ID: "a", Backend: tc.backend},
				cfg:          &config.Config{},
				resolved:     resolved,
				resolvedLive: config.NewLiveValue(resolved),
			}
			cmds, _ := registerAgentCommands(p, command.NewLastMessageStore())
			for _, name := range []string{"mana", "usage"} {
				if got := cmds.Get(name) != nil; got != tc.want {
					t.Errorf("backend %q: /%s registered = %v, want %v", tc.backend, name, got, tc.want)
				}
			}
		})
	}
}
