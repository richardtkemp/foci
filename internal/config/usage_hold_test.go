package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadTOML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte("[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path, nil)
}

// TestUsageHoldThreshold_Key is #2491's threshold key: global, default 98,
// hot, range-checked, and refused under [[agents]].
func TestUsageHoldThreshold_Key(t *testing.T) {
	f, ok := LookupField("usage_hold.threshold")
	if !ok || f.Default != "98" || f.NeedsRestart {
		t.Fatalf("usage_hold.threshold field = %+v (found %v), want default 98, hot", f, ok)
	}
	if _, ok := LookupField("agent.usage_hold.threshold"); ok {
		t.Error("an agent-level usage_hold.threshold row exists; the key is global only")
	}
	cfg, err := loadTOML(t, "[usage_hold]\nthreshold = 95\n\n[[agents]]\nid = \"a\"\n")
	if err != nil || DerefInt(cfg.UsageHold.Threshold) != 95 {
		t.Fatalf("threshold = 95: err %v, cfg %+v", err, cfg)
	}
	for _, bad := range []string{"0", "101", "-5"} {
		if _, err := loadTOML(t, "[usage_hold]\nthreshold = "+bad+"\n\n[[agents]]\nid = \"a\"\n"); err == nil || !strings.Contains(err.Error(), "1 to 100") {
			t.Errorf("threshold = %s: err %v, want a range error", bad, err)
		}
	}
	if _, err := loadTOML(t, "[[agents]]\nid = \"a\"\n[agents.usage_hold]\nthreshold = 90\n"); err == nil || !strings.Contains(err.Error(), "global only") {
		t.Errorf("agent-level usage_hold: err %v, want it refused as global only", err)
	}
}
