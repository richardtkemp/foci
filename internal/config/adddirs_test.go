package config

import (
	"slices"
	"strings"
	"testing"
)

// TestDropRelativeAddDirs (#3123): a relative backend_config.add_dirs entry is
// removed with one warning naming the agent and the entry, once per value
// across reloads; absolute entries keep their order; a slice shared with
// another agent is not changed under it.
func TestDropRelativeAddDirs(t *testing.T) {
	shared := []string{"/abs/a", "rel/b", "~/c", "/abs/d"}
	cfg := &Config{Agents: []AgentConfig{
		{ID: "t3123rel", BackendConfig: BackendConfig{AddDirs: shared}},
		{ID: "t3123abs", BackendConfig: BackendConfig{AddDirs: []string{"/x"}}},
		{ID: "t3123none"},
	}}
	warns := DropRelativeAddDirs(cfg)
	if len(warns) != 2 || !strings.Contains(warns[0], `"t3123rel"`) || !strings.Contains(warns[0], "rel/b") || !strings.Contains(warns[1], "~/c") {
		t.Fatalf("warnings %q, want two for t3123rel (rel/b, ~/c)", warns)
	}
	if got := cfg.Agents[0].BackendConfig.AddDirs; !slices.Equal(got, []string{"/abs/a", "/abs/d"}) {
		t.Errorf("t3123rel add_dirs %q, want [/abs/a /abs/d]", got)
	}
	if !slices.Equal(shared, []string{"/abs/a", "rel/b", "~/c", "/abs/d"}) {
		t.Errorf("the shared slice was changed: %q", shared)
	}
	if got := cfg.Agents[1].BackendConfig.AddDirs; !slices.Equal(got, []string{"/x"}) {
		t.Errorf("t3123abs add_dirs %q, want [/x]", got)
	}
	if got, ok := cfg.AddDirs("t3123rel"); !ok || !slices.Equal(got, []string{"/abs/a", "/abs/d"}) {
		t.Errorf("AddDirs(t3123rel) = %q, %v", got, ok)
	}
	if _, ok := cfg.AddDirs("nobody"); ok {
		t.Error("AddDirs(nobody) ok = true")
	}
	cfg.Agents[0].BackendConfig.AddDirs = shared
	if again := DropRelativeAddDirs(cfg); len(again) != 0 || len(cfg.Agents[0].BackendConfig.AddDirs) != 2 {
		t.Errorf("a reload: warnings %q (want none) add_dirs %q (want relative dropped)", again, cfg.Agents[0].BackendConfig.AddDirs)
	}
}
