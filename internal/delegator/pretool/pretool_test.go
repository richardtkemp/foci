package pretool

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func boolp(b bool) *bool { return &b }

func names(rules []Rule) []string {
	var out []string
	for _, r := range rules {
		out = append(out, r.Name)
	}
	return out
}

// TestDefaults_BlockAndPass is the per-rule contract: each preinstalled rule
// denies its own tool and leaves an ordinary tool alone.
func TestDefaults_BlockAndPass(t *testing.T) {
	rules, skipped := Resolve(Defaults)
	if len(skipped) > 0 {
		t.Fatalf("defaults skipped: %v", skipped)
	}
	for _, r := range rules {
		if got := Match(rules, Call{Tool: r.Tool, Input: json.RawMessage(`{}`)}); got == nil || got.Name != r.Name {
			t.Errorf("%s: Match(%s) = %v, want the rule", r.Name, r.Tool, got)
		}
	}
	if got := Match(rules, Call{Tool: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}); got != nil {
		t.Errorf("Bash matched %q", got.Name)
	}
}

// TestDefaults_CronReasonRedirects pins Dick's wording requirement: the
// CronCreate reason routes repeating events to crontab and one-shots to
// foci_remind.
func TestDefaults_CronReasonRedirects(t *testing.T) {
	for _, r := range Defaults {
		if r.Tool != "CronCreate" {
			continue
		}
		for _, want := range []string{"REPEATING", "crontab", "SINGLE", "foci_remind"} {
			if !strings.Contains(r.Reason, want) {
				t.Errorf("CronCreate reason missing %q: %s", want, r.Reason)
			}
		}
		return
	}
	t.Fatal("no CronCreate default")
}

func TestResolve_DisableDefault(t *testing.T) {
	rules, _ := Resolve(Defaults, []Rule{{Name: "ask_user_question", Enabled: boolp(false)}})
	if got := names(rules); !reflect.DeepEqual(got, []string{"cron_create"}) {
		t.Errorf("rules = %v, want only cron_create", got)
	}
	if Match(rules, Call{Tool: "AskUserQuestion"}) != nil {
		t.Error("disabled rule still matches")
	}
}

// TestResolve_LaterLayerWins proves field-level override across three layers
// and that a per-agent layer can re-enable what the global layer disabled.
func TestResolve_LaterLayerWins(t *testing.T) {
	global := []Rule{
		{Name: "cron_create", Reason: "global reason"},
		{Name: "ask_user_question", Enabled: boolp(false)},
	}
	agent := []Rule{{Name: "ask_user_question", Enabled: boolp(true)}}
	rules, _ := Resolve(Defaults, global, agent)
	if got := names(rules); !reflect.DeepEqual(got, []string{"ask_user_question", "cron_create"}) {
		t.Fatalf("rules = %v", got)
	}
	if rules[1].Reason != "global reason" || rules[1].Tool != "CronCreate" || rules[1].Action != ActionDeny {
		t.Errorf("override lost inherited fields: %+v", rules[1])
	}
}

// TestResolve_IncompleteRuleSkipped proves a new rule missing its tool or
// reason is reported and dropped without taking the defaults with it.
func TestResolve_IncompleteRuleSkipped(t *testing.T) {
	rules, skipped := Resolve(Defaults, []Rule{{Name: "typo", Reason: "x"}})
	if len(rules) != len(Defaults) {
		t.Errorf("rules = %v", names(rules))
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "typo") {
		t.Errorf("skipped = %v", skipped)
	}
}

// TestMatch_InputRegex is a generic (non-default) rule: a Bash command regex
// blocks the bad call and passes the normal one; a missing field never
// matches; all input constraints must hold.
func TestMatch_InputRegex(t *testing.T) {
	rules, _ := Resolve([]Rule{{
		Name:   "no_force_push",
		Tool:   "Bash",
		Input:  map[string]Patterns{"command": {`git push .*--force`}},
		Reason: "no force pushes",
	}, {
		Name:   "no_tmp_bg",
		Tool:   "Bash",
		Input:  map[string]Patterns{"command": {`^sleep`}, "run_in_background": {`^true$`}},
		Reason: "r",
	}})
	cases := []struct {
		tool, input, want string
	}{
		{"Bash", `{"command":"git push origin main --force"}`, "no_force_push"},
		{"Bash", `{"command":"git push origin main"}`, ""},
		{"Bash", `{"description":"no command field"}`, ""},
		{"Read", `{"command":"git push --force"}`, ""},
		{"Bash", `{"command":"sleep 5","run_in_background":true}`, "no_tmp_bg"},
		{"Bash", `{"command":"sleep 5","run_in_background":false}`, ""},
		{"Bash", `{"command":"sleep 5"}`, ""},
	}
	for _, c := range cases {
		got := ""
		if r := Match(rules, Call{Tool: c.tool, Input: json.RawMessage(c.input)}); r != nil {
			got = r.Name
		}
		if got != c.want {
			t.Errorf("Match(%s, %s) = %q, want %q", c.tool, c.input, got, c.want)
		}
	}
}

func TestValidateLayer(t *testing.T) {
	bad := map[string][]Rule{
		"no name":             {{Tool: "Bash"}},
		"duplicate":           {{Name: "a"}, {Name: "a"}},
		"regex tool":          {{Name: "a", Tool: "Bash|Read"}},
		"allow action":        {{Name: "a", Action: "allow"}},
		"bad regex":           {{Name: "a", Input: map[string]Patterns{"command": {"("}}}},
		"bad 2nd regex":       {{Name: "a", Input: map[string]Patterns{"command": {"ok", "("}}}},
		"bad command":         {{Name: "a", Command: Patterns{"("}}},
		"bad cwd":             {{Name: "a", Cwd: Patterns{"("}}},
		"command on non-Bash": {{Name: "a", Tool: "Read", Command: Patterns{"x"}}},
	}
	for label, rules := range bad {
		if err := ValidateLayer(rules); err == nil {
			t.Errorf("%s: accepted", label)
		}
	}
	if err := ValidateLayer([]Rule{{Name: "ask_user_question", Enabled: boolp(false)}}); err != nil {
		t.Errorf("partial override rejected: %v", err)
	}
}

// TestResolve_NeverAllow proves an "allow" action cannot survive resolution,
// even arriving via an override of a valid default.
func TestResolve_NeverAllow(t *testing.T) {
	rules, skipped := Resolve(Defaults, []Rule{{Name: "cron_create", Action: "allow"}})
	for _, r := range rules {
		if r.Action != ActionDeny {
			t.Errorf("rule %q resolved with action %q", r.Name, r.Action)
		}
	}
	if len(skipped) != 1 {
		t.Errorf("skipped = %v, want the allow override", skipped)
	}
}

func TestEncodeDecode(t *testing.T) {
	enc, err := Encode(Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(enc, " '\"$`\\;&|") {
		t.Errorf("encoding has shell metacharacters: %s", enc)
	}
	got, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, Defaults) {
		t.Errorf("round trip = %+v", got)
	}
}

func TestToolNames(t *testing.T) {
	got := ToolNames([]Rule{{Tool: "CronCreate"}, {Tool: "AskUserQuestion"}, {Tool: "CronCreate"}})
	if !reflect.DeepEqual(got, []string{"AskUserQuestion", "CronCreate"}) {
		t.Errorf("ToolNames = %v", got)
	}
}
