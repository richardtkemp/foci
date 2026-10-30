package main

import (
	"context"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/config"
)

// TestRegisterAgentCommands_ManaOnlyForBackendsWithUsage pins #1543 and
// #2658: /mana (and its /usage alias) exists only for a backend with its own
// usage source — Claude Code and Codex. An opencode agent (arnix) used to see
// it and get Claude's numbers back; it and API-mode agents still get none.
func TestRegisterAgentCommands_ManaOnlyForBackendsWithUsage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		backend string
		want    bool
	}{
		{"claude-code", true},
		{"codex", true},
		{"opencode", false},
		{"", false}, // API-mode agent
	}
	for _, tc := range cases {
		t.Run("backend="+tc.backend, func(t *testing.T) {
			cmds := registerForBackend(tc.backend, "a")
			for _, name := range []string{"mana", "usage"} {
				if got := cmds.Get(name) != nil; got != tc.want {
					t.Errorf("backend %q: /%s registered = %v, want %v", tc.backend, name, got, tc.want)
				}
			}
		})
	}
}

// TestRegisterAgentCommands_CodexUsageDescribesCodex pins #1543 for codex
// (#2658): a codex agent's /usage reply is about Codex and never carries the
// Claude Code report. It runs the real registered command end to end; with no
// codex process running in the test, codex.QueryUsage reports that it has no
// numbers yet without starting one.
func TestRegisterAgentCommands_CodexUsageDescribesCodex(t *testing.T) {
	t.Parallel()
	cmds := registerForBackend("codex", "codex-usage-"+t.Name())
	cmd := cmds.Get("usage")
	if cmd == nil {
		t.Fatal("/usage not registered for codex")
	}
	resp, err := cmd.Execute(context.Background(), command.Request{}, command.CommandContext{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(resp.Text, "*Codex usage*") {
		t.Errorf("reply does not name Codex; got:\n%s", resp.Text)
	}
	for _, claude := range []string{"Claude", "Session (5h)", "Week (7d)"} {
		if strings.Contains(resp.Text, claude) {
			t.Errorf("codex reply carries Claude Code field %q; got:\n%s", claude, resp.Text)
		}
	}
}

func registerForBackend(backend, id string) *command.Registry {
	resolved := &config.ResolvedAgentConfig{}
	p := cmdRegParams{
		ag:           &agent.Agent{},
		acfg:         config.AgentConfig{ID: id, Backend: backend},
		cfg:          &config.Config{},
		resolved:     resolved,
		resolvedLive: config.NewLiveValue(resolved),
	}
	cmds, _ := registerAgentCommands(p, command.NewLastMessageStore())
	return cmds
}
