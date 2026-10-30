package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestBackendSandboxConfig_RoundTrip verifies every supported sandbox can reach
// the backend through real agent TOML and that unset configuration stays unset.
func TestBackendSandboxConfig_RoundTrip(t *testing.T) {
	for _, mode := range []string{"", "read-only", "workspace-write", "danger-full-access"} {
		t.Run(mode, func(t *testing.T) {
			var cfg Config
			ApplyTagDefaults(&cfg)
			input := "[[agents]]\nid = 'coder'\nbackend = 'codex'\nbackend_config.sandbox = '" + mode + "'"
			md, err := toml.Decode(input, &cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(md.Undecoded()) != 0 {
				t.Fatalf("undecoded=%v", md.Undecoded())
			}
			if got := cfg.Agents[0].BackendConfig.ToMap()["sandbox"]; got != mode {
				t.Fatalf("sandbox=%v, want %q", got, mode)
			}
			if err := cfg.Validate([]string{"codex"}); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, exists := (BackendConfig{}).ToMap()["sandbox"]; exists {
		t.Fatal("unset sandbox must not override backend default")
	}
}

// TestBackendSandboxConfig_RejectsInvalidMode prevents typos from silently
// falling back to another security posture.
func TestBackendSandboxConfig_RejectsInvalidMode(t *testing.T) {
	mode := "danger-full-acess"
	cfg := Config{Agents: []AgentConfig{{ID: "coder", BackendConfig: BackendConfig{Sandbox: &mode}}}}
	if err := cfg.Validate(nil); err == nil || !strings.Contains(err.Error(), "backend_config.sandbox") {
		t.Fatalf("Validate=%v, want sandbox error", err)
	}
}
