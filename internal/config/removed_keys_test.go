package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #3110: [debug] cache_bust_detect was removed, but a config that still sets it
// (the NUC's does, globally and per agent) loads with no unknown key, so
// check-config still passes and a deploy is not aborted; one load warning
// names every place it is set.
func TestRemovedKey_CacheBustDetectStillLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foci.toml")
	content := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[debug]
cache_bust_detect = true

[[agents]]
id = "main"

[agents.debug]
cache_bust_detect = false
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.UndefinedKeys) != 0 {
		t.Errorf("UndefinedKeys = %v, want none (check-config would fail the deploy)", cfg.UndefinedKeys)
	}
	w := removedKeyWarnings(cfg)
	if len(w) != 1 {
		t.Fatalf("warnings = %q, want exactly 1", w)
	}
	for _, want := range []string{"cache_bust_detect", "[debug]", `agent "main" [debug]`, "cache_bust_notify"} {
		if !strings.Contains(w[0], want) {
			t.Errorf("warning %q lacks %q", w[0], want)
		}
	}
	if _, ok := LookupField("debug.cache_bust_detect"); ok {
		t.Error("debug.cache_bust_detect is still a settable registry row")
	}

	if w := removedKeyWarnings(&Config{Agents: []AgentConfig{{ID: "main"}}}); len(w) != 0 {
		t.Errorf("clean config warned: %q", w)
	}
}
