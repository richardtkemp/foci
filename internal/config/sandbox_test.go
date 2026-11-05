package config

import (
	"github.com/BurntSushi/toml"
	"reflect"
	"testing"
)

// TestSandboxRootsCascade verifies TOML decoding, global inheritance, replacement,
// and explicit empty-list removal independently of the selected backend.
func TestSandboxRootsCascade(t *testing.T) {
	var cfg Config
	_, err := toml.Decode(`[sandbox]
writable_roots = ["/global"]
[[agents]]
id = "inherited"
backend = "codex"
[[agents]]
id = "replaced"
backend = "claude-code"
[agents.sandbox]
writable_roots = ["/agent"]
[[agents]]
id = "cleared"
backend = "opencode"
[agents.sandbox]
writable_roots = []
`, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range [][]string{{"/global"}, {"/agent"}, {}} {
		if got := Resolve(&cfg, cfg.Agents[i]).Sandbox.WritableRoots; !reflect.DeepEqual(got, want) {
			t.Errorf("agent %d: %v want %v", i, got, want)
		}
	}
}

// TestSandboxRootsValidation rejects ambiguous grants in either scope, before a
// backend starts, without requiring that paths already exist on disk.
func TestSandboxRootsValidation(t *testing.T) {
	for _, path := range []string{"", "relative", "~/gcloud", "/bad\x00path"} {
		for _, agent := range []bool{false, true} {
			cfg := Config{}
			if agent {
				cfg.Agents = []AgentConfig{{Sandbox: SandboxConfig{WritableRoots: []string{path}}}}
			} else {
				cfg.Sandbox.WritableRoots = []string{path}
			}
			if err := cfg.Validate(nil); err == nil {
				t.Errorf("path=%q agent=%v accepted", path, agent)
			}
		}
	}
	if err := validateWritableRoots("sandbox", []string{"/absolute/not-yet-created", "/path with spaces"}); err != nil {
		t.Fatal(err)
	}
}
