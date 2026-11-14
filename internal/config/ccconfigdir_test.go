package config

import (
	"strings"
	"testing"
)

// TestDropRelativeCCConfigDirs (#3104 phase 2, note b): a relative
// CLAUDE_CONFIG_DIR is removed from the agent's env with one warning naming
// the agent, once per value across reloads; an absolute one and the other
// keys stay; a map shared with another agent is not changed under it.
func TestDropRelativeCCConfigDirs(t *testing.T) {
	shared := map[string]string{"CLAUDE_CONFIG_DIR": "rel/cc", "OTHER": "1"}
	cfg := &Config{Agents: []AgentConfig{
		{ID: "t3104rel", BackendConfig: BackendConfig{Env: shared}},
		{ID: "t3104abs", BackendConfig: BackendConfig{Env: map[string]string{"CLAUDE_CONFIG_DIR": "/abs/cc"}}},
		{ID: "t3104none"},
	}}
	warns := DropRelativeCCConfigDirs(cfg)
	if len(warns) != 1 || !strings.Contains(warns[0], `"t3104rel"`) || !strings.Contains(warns[0], "rel/cc") {
		t.Fatalf("warnings %q, want one for t3104rel", warns)
	}
	if env := cfg.Agents[0].BackendConfig.Env; env["CLAUDE_CONFIG_DIR"] != "" || env["OTHER"] != "1" {
		t.Errorf("t3104rel env %v, want CLAUDE_CONFIG_DIR gone and OTHER kept", env)
	}
	if shared["CLAUDE_CONFIG_DIR"] != "rel/cc" {
		t.Error("the shared map was changed")
	}
	if cfg.Agents[1].BackendConfig.Env["CLAUDE_CONFIG_DIR"] != "/abs/cc" {
		t.Error("an absolute folder was dropped")
	}
	cfg.Agents[0].BackendConfig.Env = shared
	if again := DropRelativeCCConfigDirs(cfg); len(again) != 0 || cfg.Agents[0].BackendConfig.Env["CLAUDE_CONFIG_DIR"] != "" {
		t.Errorf("a reload: warnings %q (want none) env %v (want dropped)", again, cfg.Agents[0].BackendConfig.Env)
	}
}
