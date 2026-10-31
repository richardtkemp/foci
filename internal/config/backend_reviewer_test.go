package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestBackendReviewerConfig_RoundTrip exercises the real TOML-to-backend path
// without replacing an omitted value with an explicit user override.
func TestBackendReviewerConfig_RoundTrip(t *testing.T) {
	for _, mode := range []string{"", "auto_review", "user"} {
		t.Run(mode, func(t *testing.T) {
			var cfg Config
			ApplyTagDefaults(&cfg)
			md, err := toml.Decode("[[agents]]\nid='coder'\nbackend='codex'\nbackend_config.approvals_reviewer='"+mode+"'", &cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(md.Undecoded()) != 0 {
				t.Fatalf("undecoded=%v", md.Undecoded())
			}
			if got := cfg.Agents[0].BackendConfig.ToMap()["approvals_reviewer"]; got != mode {
				t.Fatalf("reviewer=%v, want %q", got, mode)
			}
			if err := cfg.Validate([]string{"codex"}); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, exists := (BackendConfig{}).ToMap()["approvals_reviewer"]; exists {
		t.Fatal("unset reviewer must retain the backend default")
	}
}

// TestBackendReviewerConfig_RejectsInvalidMode catches misspellings instead of
// silently routing approvals to a different reviewer.
func TestBackendReviewerConfig_RejectsInvalidMode(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode("[[agents]]\nid='coder'\nbackend_config.approvals_reviewer='auto'", &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(nil); err == nil || !strings.Contains(err.Error(), "backend_config.approvals_reviewer") {
		t.Fatalf("Validate=%v, want reviewer error", err)
	}
}
