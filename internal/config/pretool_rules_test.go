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

// TestLoad_PreToolRuleExamples proves both config layers decode the two
// example forms (#2039) — the plain string and the Bash-only {cmd, cwd}
// table — and that rules (and configs) without the fields load exactly as
// before, with no example fields set.
func TestLoad_PreToolRuleExamples(t *testing.T) {
	cfg, err := loadPretoolFixture(t, `
[[cc_backend.pretool_rules]]
name = "no_rm"
tool = "Bash"
command = '(sudo (\S+ )*)?rm( |$)'
deny_examples  = ["rm -rf /tmp/x", "sudo rm /x"]
allow_examples = ["trash /tmp/x", { cmd = "git rm --cached f", cwd = "/tmp" }]
reason = "no rm"

[[agents]]
id = "a"

[[agents.backend_config.pretool_rules]]
name = "no_edit"
tool = "Edit"
input.file_path = '^/etc/'
deny_examples  = ['{"file_path":"/etc/passwd"}']
allow_examples = ['{"file_path":"/home/x"}']
reason = "r"
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	g := cfg.CCBackend.PreToolRules[0]
	if !reflect.DeepEqual(g.DenyExamples, pretool.Examples{{Text: "rm -rf /tmp/x"}, {Text: "sudo rm /x"}}) {
		t.Errorf("global deny_examples = %+v", g.DenyExamples)
	}
	want := []pretool.Example{{Text: "trash /tmp/x"}, {Text: "git rm --cached f", Cwd: "/tmp"}}
	if len(g.AllowExamples) != len(want) {
		t.Fatalf("global allow_examples = %+v, want %+v", g.AllowExamples, want)
	}
	for i, w := range want {
		if got := g.AllowExamples[i]; got.Text != w.Text || got.Cwd != w.Cwd {
			t.Errorf("allow_examples[%d] = %+v, want %+v", i, got, w)
		}
	}
	a := cfg.Agents[0].BackendConfig.PreToolRules[0]
	if !reflect.DeepEqual(a.DenyExamples, pretool.Examples{{Text: `{"file_path":"/etc/passwd"}`}}) {
		t.Errorf("agent deny_examples = %+v", a.DenyExamples)
	}

	cfg, err = loadPretoolFixture(t, `
[[cc_backend.pretool_rules]]
name = "no_rm"
tool = "Bash"
command = 'rm( |$)'
reason = "no rm"

[[agents]]
id = "a"
`)
	if err != nil {
		t.Fatalf("Load without examples: %v", err)
	}
	if r := cfg.CCBackend.PreToolRules[0]; r.DenyExamples != nil || r.AllowExamples != nil {
		t.Errorf("rule without example fields = %+v %+v", r.DenyExamples, r.AllowExamples)
	}
	rules, skipped, ok := cfg.PreToolRules("a")
	if !ok || len(skipped) != 0 || len(rules) != 3 {
		t.Errorf("PreToolRules = %v skipped %v ok %v, want the two defaults plus no_rm", namesOf(rules), skipped, ok)
	}
}

// namesOf is pretool.names for this package (Resolve output order).
func namesOf(rules []pretool.Rule) []string {
	var out []string
	for _, r := range rules {
		out = append(out, r.Name)
	}
	return out
}

// TestLoad_PreToolRuleExamplesRejectMalformed proves a malformed example is
// a config load error naming the rule, at either layer and — via the
// resolved-set pass — when an override layer misshapes an example only
// after the merge.
func TestLoad_PreToolRuleExamplesRejectMalformed(t *testing.T) {
	for label, body := range map[string]string{
		"global table on non-Bash": `
[[cc_backend.pretool_rules]]
name = "no_edit"
tool = "Edit"
input.file_path = '^/etc/'
deny_examples = [{ cmd = "rm x" }]
reason = "r"

[[agents]]
id = "a"
`,
		"global non-JSON string": `
[[cc_backend.pretool_rules]]
name = "no_edit"
tool = "Edit"
input.file_path = '^/etc/'
allow_examples = ["/etc/passwd"]
reason = "r"

[[agents]]
id = "a"
`,
		"agent table without cmd": `
[[agents]]
id = "a"

[[agents.backend_config.pretool_rules]]
name = "no_rm"
tool = "Bash"
command = 'rm( |$)'
deny_examples = [{ cwd = "/tmp" }]
reason = "r"
`,
		"agent unknown table key": `
[[agents]]
id = "a"

[[agents.backend_config.pretool_rules]]
name = "no_rm"
tool = "Bash"
command = 'rm( |$)'
deny_examples = [{ command = "rm x" }]
reason = "r"
`,
		"override layer after merge": `
[[cc_backend.pretool_rules]]
name = "no_edit"
tool = "Edit"
input.file_path = '^/etc/'
reason = "r"

[[agents]]
id = "a"

[[agents.backend_config.pretool_rules]]
name = "no_edit"
deny_examples = [{ cmd = "rm x" }]
`,
	} {
		_, err := loadPretoolFixture(t, body)
		if err == nil || !strings.Contains(err.Error(), "no_edit") && !strings.Contains(err.Error(), "no_rm") {
			t.Errorf("%s: err = %v, want a rejection naming the rule", label, err)
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
