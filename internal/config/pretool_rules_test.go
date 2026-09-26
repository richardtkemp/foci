package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"foci/internal/delegator/pretool"
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

// TestLoad_PreToolRulesRejectUnknownKeys proves a rule key the decoder does
// not know fails the load (#2047). Ignoring it left the rule with fewer
// constraints than written, so a typo in a rule's only constraint made it
// deny every call of its tool.
func TestLoad_PreToolRulesRejectUnknownKeys(t *testing.T) {
	for label, body := range map[string]string{
		"global": `
[[cc_backend.pretool_rules]]
name = "x"
tool = "Bash"
backgrund = true
reason = "r"
`,
		"global inline": `
[cc_backend]
pretool_rules = [{ name = "x", tool = "Bash", backgrund = true, reason = "r" }]
`,
		"agent": `
[[agents]]
id = "a"

[[agents.backend_config.pretool_rules]]
name = "x"
tool = "Bash"
backgrund = true
reason = "r"
`,
		"agent inline": `
[[agents]]
id = "a"
backend_config.pretool_rules = [{ name = "x", tool = "Bash", backgrund = true, reason = "r" }]
`,
	} {
		_, err := loadPretoolFixture(t, body)
		if err == nil || !strings.Contains(err.Error(), "backgrund") {
			t.Errorf("%s: err = %v, want an unknown-key rejection naming backgrund", label, err)
		}
	}
}

// TestLoad_PreToolRulesAcceptEveryRuleKey proves the unknown-key check
// accepts every key pretool.Rule decodes. The keys come from the struct's
// toml tags, so a field added later is covered without editing this test.
func TestLoad_PreToolRulesAcceptEveryRuleKey(t *testing.T) {
	values := map[reflect.Kind]string{
		reflect.String: `"Bash"`,
		reflect.Ptr:    "false",
		reflect.Slice:  `["x"]`,
		reflect.Map:    `{ command = "x" }`,
	}
	var lines []string
	rt := reflect.TypeOf(pretool.Rule{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		key := strings.Split(f.Tag.Get("toml"), ",")[0]
		if key == "" || key == "-" {
			continue
		}
		v, ok := values[f.Type.Kind()]
		if !ok {
			t.Fatalf("field %s: no fixture value for kind %s; add one", f.Name, f.Type.Kind())
		}
		switch key {
		case "name":
			v = `"every_key"`
		case "action":
			v = `"deny"`
		case "when":
			v = `"exit 1"`
		}
		lines = append(lines, key+" = "+v)
	}
	body := "\n[[agents]]\nid = \"a\"\n\n[[agents.backend_config.pretool_rules]]\n" + strings.Join(lines, "\n") + "\n"
	cfg, err := loadPretoolFixture(t, body)
	if err != nil {
		t.Fatalf("Load with every rule key:\n%s\nerr: %v", body, err)
	}
	if len(cfg.UndefinedKeys) != 0 {
		t.Errorf("UndefinedKeys = %v, want none", cfg.UndefinedKeys)
	}
}
