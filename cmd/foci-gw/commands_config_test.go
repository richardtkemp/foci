package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/config"
)

// TestRegisterAgentCommandsConfigGetRunningValue proves the gateway wiring
// of /config get's running rung: AgentRunningValueFn resolves the ASKED
// agent's row in the startup config (any agent id, not just the registry's
// own), so `get <agent> <key>` reports that agent's running value with the
// running source when the file has none — while an id that validates as a
// running agent but has no row in the startup config skips the rung and
// lands on the built-in default.
func TestRegisterAgentCommandsConfigGetRunningValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte("[[agents]]\nid = \"a\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved := &config.ResolvedAgentConfig{}
	p := cmdRegParams{
		ag:         &agent.Agent{},
		acfg:       config.AgentConfig{ID: "main"},
		configPath: path,
		cfg: &config.Config{
			SourcePath: path,
			Agents: []config.AgentConfig{
				{ID: "a", Keepalive: config.KeepaliveConfig{Interval: ptr("42m")}},
			},
		},
		resolved:     resolved,
		resolvedLive: config.NewLiveValue(resolved),
		agentListFn: func() []command.AgentInfo {
			return []command.AgentInfo{{ID: "a"}, {ID: "ghost"}}
		},
	}
	cmds, cc := registerAgentCommands(p, command.NewLastMessageStore())
	configCmd := cmds.Get("config")
	if configCmd == nil {
		t.Fatal("config command not registered")
	}

	resp, err := configCmd.Execute(context.Background(), command.Request{Name: "config", Args: "get a keepalive.interval"}, cc)
	if err != nil {
		t.Fatalf("get a keepalive.interval: %v", err)
	}
	for _, want := range []string{"42m", "Source: running config (not set in foci.toml)"} {
		if !strings.Contains(resp.Text, want) {
			t.Errorf("reply %q missing %q", resp.Text, want)
		}
	}

	resp, err = configCmd.Execute(context.Background(), command.Request{Name: "config", Args: "get ghost keepalive.interval"}, cc)
	if err != nil {
		t.Fatalf("get ghost keepalive.interval: %v", err)
	}
	for _, want := range []string{"55m", "built-in default"} {
		if !strings.Contains(resp.Text, want) {
			t.Errorf("ghost reply %q missing %q", resp.Text, want)
		}
	}
	if strings.Contains(resp.Text, "running config") {
		t.Errorf("ghost reply must not claim a running value: %q", resp.Text)
	}
}

// TestRegisterAgentCommandsConfigGetOverrideField proves /config get
// AGREES with the app config editor on override fields (review round 1,
// Finding 1): sessions.max_system_prompt_chars_file carries its default
// only on the global [sessions] struct — the agent's row alone reads ""
// — so the running rung must supply the mapped global section's running
// value (20000 here), the same value internal/app's
// TestConfigGet_AgentScopeSharedRunningRung pins on the editor's side.
// Before
// the rung was unified, get replied "(not set)" / built-in default here
// while the editor showed 20000 for the very same agent and config.
func TestRegisterAgentCommandsConfigGetOverrideField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte("[[agents]]\nid = \"a\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved := &config.ResolvedAgentConfig{}
	p := cmdRegParams{
		ag:         &agent.Agent{},
		acfg:       config.AgentConfig{ID: "main"},
		configPath: path,
		cfg: &config.Config{
			SourcePath: path,
			// As Load() would default it: the global struct holds the 20000
			// the agent's override row lacks.
			Sessions: config.SessionsConfig{MaxSystemPromptFile: 20000},
			Agents:   []config.AgentConfig{{ID: "a"}},
		},
		resolved:     resolved,
		resolvedLive: config.NewLiveValue(resolved),
		agentListFn:  func() []command.AgentInfo { return []command.AgentInfo{{ID: "a"}} },
	}
	cmds, cc := registerAgentCommands(p, command.NewLastMessageStore())
	configCmd := cmds.Get("config")
	if configCmd == nil {
		t.Fatal("config command not registered")
	}

	resp, err := configCmd.Execute(context.Background(), command.Request{Name: "config", Args: "get a sessions.max_system_prompt_chars_file"}, cc)
	if err != nil {
		t.Fatalf("get a sessions.max_system_prompt_chars_file: %v", err)
	}
	for _, want := range []string{"20000", "Source: running config (not set in foci.toml)"} {
		if !strings.Contains(resp.Text, want) {
			t.Errorf("reply %q missing %q", resp.Text, want)
		}
	}
	for _, stale := range []string{"(not set)", "built-in default"} {
		if strings.Contains(resp.Text, stale) {
			t.Errorf("reply must report the running global value the server actually uses, found %q: %q", stale, resp.Text)
		}
	}
}
