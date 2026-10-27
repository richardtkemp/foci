package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_CCBatchModelDefault proves [cc_backend] batch_model defaults to
// haiku when the key is absent — so foci's own summaries stay on haiku now
// that Claude Code has no built-in cheap batch model — while an explicit
// value, including "" (the agent's own model), is kept as written.
func TestLoad_CCBatchModelDefault(t *testing.T) {
	cases := []struct {
		name, section, want string
	}{
		{"absent", "", DefaultCCBatchModel},
		{"explicit model", "[cc_backend]\nbatch_model = \"sonnet\"\n", "sonnet"},
		{"explicit empty", "[cc_backend]\nbatch_model = \"\"\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "foci.toml")
			body := c.section + "\n[[agents]]\nid = \"test\"\nbackend = \"claude-code\"\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path, []string{"claude-code"})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.CCBackend.BatchModel != c.want {
				t.Errorf("cc_backend batch_model = %q, want %q", cfg.CCBackend.BatchModel, c.want)
			}
		})
	}
}
