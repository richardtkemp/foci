package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pretoolFixtureBase = `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"
`

func loadPretoolFixture(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte(pretoolFixtureBase+body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path, nil)
}

// TestLoad_PreToolRules proves both config layers decode, including the
// input table and the enabled switch.
func TestLoad_PreToolRules(t *testing.T) {
	cfg, err := loadPretoolFixture(t, `
[[cc_backend.pretool_rules]]
name = "no_force_push"
tool = "Bash"
input = { command = "git push .*--force" }
reason = "no force pushes"

[[agents]]
id = "a"

[[agents.backend_config.pretool_rules]]
name = "ask_user_question"
enabled = false
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	g := cfg.CCBackend.PreToolRules
	if len(g) != 1 || g[0].Tool != "Bash" || len(g[0].Input["command"]) != 1 || g[0].Input["command"][0] != "git push .*--force" || g[0].Reason != "no force pushes" {
		t.Errorf("global rules = %+v", g)
	}
	a := cfg.Agents[0].BackendConfig.PreToolRules
	if len(a) != 1 || a[0].Name != "ask_user_question" || a[0].Enabled == nil || *a[0].Enabled {
		t.Errorf("agent rules = %+v", a)
	}
}

// TestLoad_PreToolRulesRejectsAllow proves a rule can't be configured to
// allow (which would bypass foci's permission flow), at either layer.
func TestLoad_PreToolRulesRejectsAllow(t *testing.T) {
	for label, body := range map[string]string{
		"global": `
[[cc_backend.pretool_rules]]
name = "x"
tool = "Bash"
action = "allow"
reason = "r"
`,
		"agent": `
[[agents]]
id = "a"

[[agents.backend_config.pretool_rules]]
name = "x"
action = "allow"
`,
	} {
		_, err := loadPretoolFixture(t, body)
		if err == nil || !strings.Contains(err.Error(), "allow") {
			t.Errorf("%s: err = %v, want an action rejection", label, err)
		}
	}
}
