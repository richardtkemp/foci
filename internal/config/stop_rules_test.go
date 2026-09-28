package config

import (
	"reflect"
	"strings"
	"testing"

	"foci/internal/delegator/stoprule"
)

// TestLoad_StopRules proves the per-agent layer decodes and resolves, with
// the enabled switch honoured and no rules for an agent that sets none.
func TestLoad_StopRules(t *testing.T) {
	cfg, err := loadPretoolFixture(t, `
[[agents]]
id = "a"

[[agents.backend_config.stop_rules]]
name = "announce_start"
text = ['(?i)\bI am (now )?starting\b', '(?i)\bstarting .* now\b']
reason = "Start it or say it is deferred."

[[agents.backend_config.stop_rules]]
name = "off"
text = "x"
enabled = false

[[agents]]
id = "b"
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rules, skipped, ok := cfg.StopRules("a")
	if !ok || len(skipped) != 0 || len(rules) != 1 || rules[0].Name != "announce_start" || len(rules[0].Text) != 2 {
		t.Errorf("StopRules(a) = %+v skipped=%v ok=%v", rules, skipped, ok)
	}
	if rules, _, ok := cfg.StopRules("b"); !ok || len(rules) != 0 {
		t.Errorf("StopRules(b) = %+v, want none (no defaults ship)", rules)
	}
}

// TestLoad_StopRulesRejectBadRules proves validation rejects each kind of
// bad rule at load: missing name, missing text, empty or invalid regex,
// duplicate name, an unknown key, and a global [cc_backend] layer.
func TestLoad_StopRulesRejectBadRules(t *testing.T) {
	agent := "\n[[agents]]\nid = \"a\"\n"
	for label, tc := range map[string]struct{ body, want string }{
		"no name":      {agent + "[[agents.backend_config.stop_rules]]\ntext = \"x\"\n", "name is required"},
		"no text":      {agent + "[[agents.backend_config.stop_rules]]\nname = \"n\"\n", "text is required"},
		"empty regex":  {agent + "[[agents.backend_config.stop_rules]]\nname = \"n\"\ntext = \"\"\n", "empty pattern"},
		"bad regex":    {agent + "[[agents.backend_config.stop_rules]]\nname = \"n\"\ntext = \"(\"\n", "missing closing )"},
		"duplicate":    {agent + "[[agents.backend_config.stop_rules]]\nname = \"n\"\ntext = \"x\"\n[[agents.backend_config.stop_rules]]\nname = \"n\"\ntext = \"y\"\n", "duplicate name"},
		"unknown key":  {agent + "[[agents.backend_config.stop_rules]]\nname = \"n\"\ntxt = \"x\"\n", "txt"},
		"inline typo":  {agent + "backend_config.stop_rules = [{ name = \"n\", text = \"x\", reasn = \"r\" }]\n", "reasn"},
		"global layer": {"\n[[cc_backend.stop_rules]]\nname = \"n\"\ntext = \"x\"\n", "per-agent only"},
	} {
		_, err := loadPretoolFixture(t, tc.body)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one containing %q", label, err, tc.want)
		}
	}
}

// TestLoad_StopRulesAcceptEveryRuleKey proves the unknown-key check accepts
// every key stoprule.Rule decodes, read from its toml tags.
func TestLoad_StopRulesAcceptEveryRuleKey(t *testing.T) {
	values := map[string]string{"name": `"every_key"`, "text": `["x"]`, "reason": `"r"`, "enabled": "true"}
	var lines []string
	rt := reflect.TypeOf(stoprule.Rule{})
	for i := 0; i < rt.NumField(); i++ {
		key := strings.Split(rt.Field(i).Tag.Get("toml"), ",")[0]
		v, ok := values[key]
		if !ok {
			t.Fatalf("field %s (toml %q): no fixture value; add one", rt.Field(i).Name, key)
		}
		lines = append(lines, key+" = "+v)
	}
	body := "\n[[agents]]\nid = \"a\"\n\n[[agents.backend_config.stop_rules]]\n" + strings.Join(lines, "\n") + "\n"
	cfg, err := loadPretoolFixture(t, body)
	if err != nil {
		t.Fatalf("Load with every rule key:\n%s\nerr: %v", body, err)
	}
	if len(cfg.UndefinedKeys) != 0 {
		t.Errorf("UndefinedKeys = %v, want none", cfg.UndefinedKeys)
	}
}
