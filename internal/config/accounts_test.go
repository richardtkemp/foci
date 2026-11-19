package config

import (
	"strings"
	"testing"
)

// TestNormalizeAccounts (#3186 P5): backend_config.accounts is the only way
// to name an agent's Claude Code account. The env form is removed with one
// warning that gives the line to write instead, once across reloads; a
// relative entry is dropped; a list of several keeps its first; a map shared
// with another agent is not changed under it.
func TestNormalizeAccounts(t *testing.T) {
	shared := map[string]string{"CLAUDE_CONFIG_DIR": "/abs/cc", "OTHER": "1"}
	cfg := &Config{Agents: []AgentConfig{
		{ID: "t3186env", BackendConfig: BackendConfig{Env: shared}},
		{ID: "t3186list", BackendConfig: BackendConfig{Accounts: []string{"/abs/one"}}},
		{ID: "t3186rel", BackendConfig: BackendConfig{Accounts: []string{"rel/cc"}}},
		{ID: "t3186many", BackendConfig: BackendConfig{Accounts: []string{"/abs/a", "/abs/b"}}},
		{ID: "t3186secure", BackendConfig: BackendConfig{Env: map[string]string{"CLAUDE_SECURESTORAGE_CONFIG_DIR": ""}}},
		{ID: "t3186none"},
	}}
	warns := NormalizeAccounts(cfg)
	if len(warns) != 4 {
		t.Fatalf("warnings %q, want 4 (env, relative, several, secure storage)", warns)
	}
	if w := warns[0]; !strings.Contains(w, `"t3186env"`) || !strings.Contains(w, `backend_config.accounts = ["/abs/cc"]`) {
		t.Errorf("env warning %q does not say what to write instead", w)
	}
	if env := cfg.Agents[0].BackendConfig.Env; env["CLAUDE_CONFIG_DIR"] != "" || env["OTHER"] != "1" {
		t.Errorf("t3186env env %v, want CLAUDE_CONFIG_DIR gone and OTHER kept", env)
	}
	if len(cfg.Agents[0].BackendConfig.Accounts) != 0 {
		t.Errorf("the env form still named an account: %v", cfg.Agents[0].BackendConfig.Accounts)
	}
	if shared["CLAUDE_CONFIG_DIR"] != "/abs/cc" {
		t.Error("the shared map was changed")
	}
	if got := cfg.Agents[1].BackendConfig.Accounts; len(got) != 1 || got[0] != "/abs/one" {
		t.Errorf("t3186list accounts %v, want kept", got)
	}
	if got := cfg.Agents[2].BackendConfig.Accounts; len(got) != 0 {
		t.Errorf("t3186rel accounts %v, want the relative entry dropped", got)
	}
	if got := cfg.Agents[3].BackendConfig.Accounts; len(got) != 1 || got[0] != "/abs/a" {
		t.Errorf("t3186many accounts %v, want only the first", got)
	}
	if _, ok := cfg.Agents[4].BackendConfig.Env["CLAUDE_SECURESTORAGE_CONFIG_DIR"]; ok {
		t.Error("CLAUDE_SECURESTORAGE_CONFIG_DIR stayed in the env")
	}
	cfg.Agents[0].BackendConfig.Env = shared
	cfg.Agents[3].BackendConfig.Accounts = []string{"/abs/a", "/abs/b"}
	if again := NormalizeAccounts(cfg); len(again) != 0 {
		t.Errorf("a reload repeated warnings: %q", again)
	}
	if cfg.Agents[0].BackendConfig.Env["CLAUDE_CONFIG_DIR"] != "" || len(cfg.Agents[3].BackendConfig.Accounts) != 1 {
		t.Error("a reload did not normalise again")
	}
}
