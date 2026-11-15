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
// bad rule at load, in either layer: missing name, missing text (after the
// merge), empty or invalid regex, duplicate name and an unknown key.
func TestLoad_StopRulesRejectBadRules(t *testing.T) {
	agent := "\n[[agents]]\nid = \"a\"\n"
	for label, tc := range map[string]struct{ body, want string }{
		"no name":            {agent + "[[agents.backend_config.stop_rules]]\ntext = \"x\"\n", "name is required"},
		"no text":            {agent + "[[agents.backend_config.stop_rules]]\nname = \"n\"\n", "text is required"},
		"empty regex":        {agent + "[[agents.backend_config.stop_rules]]\nname = \"n\"\ntext = \"\"\n", "empty pattern"},
		"bad regex":          {agent + "[[agents.backend_config.stop_rules]]\nname = \"n\"\ntext = \"(\"\n", "missing closing )"},
		"duplicate":          {agent + "[[agents.backend_config.stop_rules]]\nname = \"n\"\ntext = \"x\"\n[[agents.backend_config.stop_rules]]\nname = \"n\"\ntext = \"y\"\n", "duplicate name"},
		"unknown key":        {agent + "[[agents.backend_config.stop_rules]]\nname = \"n\"\ntxt = \"x\"\n", "txt"},
		"inline typo":        {agent + "backend_config.stop_rules = [{ name = \"n\", text = \"x\", reasn = \"r\" }]\n", "reasn"},
		"global no text":     {"\n[[cc_backend.stop_rules]]\nname = \"n\"\n" + agent, "text is required"},
		"global bad regex":   {"\n[[cc_backend.stop_rules]]\nname = \"n\"\ntext = \"(\"\n", "missing closing )"},
		"global unknown key": {"\n[[cc_backend.stop_rules]]\nname = \"n\"\ntxt = \"x\"\n", "txt"},
		"global duplicate":   {"\n[[cc_backend.stop_rules]]\nname = \"n\"\ntext = \"x\"\n[[cc_backend.stop_rules]]\nname = \"n\"\ntext = \"y\"\n", "duplicate name"},
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

// TestLoad_StopRulesGlobalLayer proves [[cc_backend.stop_rules]] applies to
// every agent and merges by name with the agent's layer (#3124): an agent
// entry gives only the fields it changes (a new reason, enabled = false), a
// disabled global rule can be switched back on, and an agent's own rules are
// added after the global ones.
func TestLoad_StopRulesGlobalLayer(t *testing.T) {
	cfg, err := loadPretoolFixture(t, `
[cc_backend]
stop_rules = [
  { name = "announce_start", text = ['(?i)\bstarting .* now\b'], reason = "global reason" },
  { name = "dormant", text = "later", enabled = false },
]

[[agents]]
id = "plain"

[[agents]]
id = "reworded"
backend_config.stop_rules = [
  { name = "announce_start", reason = "agent reason" },
  { name = "own", text = "mine" },
]

[[agents]]
id = "optout"
backend_config.stop_rules = [{ name = "announce_start", enabled = false }]

[[agents]]
id = "wake"
backend_config.stop_rules = [{ name = "dormant", enabled = true }]
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	get := func(id string) []stoprule.Rule {
		t.Helper()
		rules, skipped, ok := cfg.StopRules(id)
		if !ok || len(skipped) != 0 {
			t.Fatalf("StopRules(%s): ok=%v skipped=%v", id, ok, skipped)
		}
		return rules
	}
	if r := get("plain"); len(r) != 1 || r[0].Name != "announce_start" || r[0].Reason != "global reason" || len(r[0].Text) != 1 {
		t.Errorf("plain = %+v, want the global announce_start only", r)
	}
	if r := get("reworded"); len(r) != 2 || r[0].Name != "announce_start" || r[0].Reason != "agent reason" ||
		len(r[0].Text) != 1 || r[0].Text[0] != `(?i)\bstarting .* now\b` || r[1].Name != "own" {
		t.Errorf("reworded = %+v, want announce_start (global text, agent reason) then own", r)
	}
	if r := get("optout"); len(r) != 0 {
		t.Errorf("optout = %+v, want none", r)
	}
	if r := get("wake"); len(r) != 2 || r[1].Name != "dormant" || r[1].Text[0] != "later" {
		t.Errorf("wake = %+v, want announce_start and the re-enabled dormant", r)
	}
}
