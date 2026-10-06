package pretool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestExamplesUnmarshalTOML proves the two entry forms decode — a plain
// string, and the Bash-only {cmd, cwd} table — and that anything else is a
// decode error, not a silently dropped example.
func TestExamplesUnmarshalTOML(t *testing.T) {
	var e Examples
	if err := e.UnmarshalTOML([]any{"rm -rf /tmp/x", map[string]any{"cmd": "git rm --cached f", "cwd": "/tmp"}}); err != nil {
		t.Fatalf("mixed list: %v", err)
	}
	want := Examples{{Text: "rm -rf /tmp/x"}, {Text: "git rm --cached f", Cwd: "/tmp", table: true}}
	if !reflect.DeepEqual(e, want) {
		t.Errorf("decoded %+v, want %+v", e, want)
	}
	if got := e[1].String(); got != "git rm --cached f (cwd: /tmp)" {
		t.Errorf("table display = %q", got)
	}
	for label, v := range map[string]any{
		"non-array":     "rm -rf /tmp/x",
		"non-string":    []any{42},
		"non-cmd value": []any{map[string]any{"cmd": 42}},
		"non-cwd value": []any{map[string]any{"cmd": "x", "cwd": 42}},
	} {
		var e Examples
		if err := e.UnmarshalTOML(v); err == nil {
			t.Errorf("%s: accepted", label)
		}
	}
}

// TestValidateRuleExamples proves every malformed example is rejected with
// an error naming the rule, while the two Bash forms are accepted. A tool
// of "" (a pure override layer) allows the tool-dependent checks to wait
// for the resolved-set pass.
func TestValidateRuleExamples(t *testing.T) {
	ok := []Rule{{
		Name:          "no_rm",
		Tool:          "Bash",
		Command:       Patterns{"rm( |$)"},
		DenyExamples:  Examples{{Text: "rm -rf /tmp/x"}, {Text: "git rm --cached f", Cwd: "/tmp", table: true}},
		AllowExamples: Examples{{Text: "trash /tmp/x"}},
	}}
	for _, r := range ok {
		if err := ValidateRuleExamples(r.Tool, r.Name, r.DenyExamples, r.AllowExamples); err != nil {
			t.Errorf("valid rule rejected: %v", err)
		}
	}
	bad := map[string]Rule{
		"table without cmd": {Name: "no_rm", Tool: "Bash", DenyExamples: Examples{{Cwd: "/tmp", table: true}}},
		"empty cmd":         {Name: "no_rm", Tool: "Bash", AllowExamples: Examples{{Text: "  ", table: true}}},
		"unknown table key": {Name: "no_rm", Tool: "Bash", DenyExamples: Examples{{Text: "rm x", Cwd: "/tmp", table: true, unknown: []string{"cwdd"}}}},
		"table on non-Bash": {Name: "no_edit", Tool: "Edit", DenyExamples: Examples{{Text: "rm x", table: true}}},
		"non-JSON string":   {Name: "no_edit", Tool: "Edit", AllowExamples: Examples{{Text: "/etc/passwd"}}},
		"override no cmd":   {Name: "no_rm", DenyExamples: Examples{{table: true}}},
	}
	for label, r := range bad {
		err := ValidateRuleExamples(r.Tool, r.Name, r.DenyExamples, r.AllowExamples)
		if err == nil || !strings.Contains(err.Error(), r.Name) {
			t.Errorf("%s: err = %v, want a rejection naming %q", label, err, r.Name)
		}
	}
	// The override layer keeps the tool-dependent checks waiting: a table
	// example and a non-JSON string are layer-accepted, and caught on the
	// resolved set instead.
	r := Rule{Name: "no_rm", DenyExamples: Examples{{Text: "rm x", table: true}}, AllowExamples: Examples{{Text: "not json"}}}
	if err := ValidateRuleExamples("", r.Name, r.DenyExamples, r.AllowExamples); err != nil {
		t.Errorf("override layer rejected: %v", err)
	}
}

// TestRunExamplesDeny is the deny contract: an example passes only if the
// resolved set's matching rule is its own; a different rule matching first
// fails it naming that rule, and so does nothing matching.
func TestRunExamplesDeny(t *testing.T) {
	rules := []Rule{
		{Name: "no_sudo", Tool: "Bash", Action: ActionDeny, Command: Patterns{"sudo( |$)"}, Reason: "r"},
		{Name: "no_rm", Tool: "Bash", Action: ActionDeny, Command: Patterns{"rm( |$)"}, Reason: "r",
			DenyExamples: Examples{{Text: "rm -rf /tmp/x"}, {Text: "sudo rm /x"}, {Text: "ls"}}},
	}
	outcomes := RunExamples(rules, WhenRun)
	if len(outcomes) != 3 {
		t.Fatalf("outcomes = %+v, want 3", outcomes)
	}
	cases := []struct {
		ex, matched string
		pass        bool
	}{
		{"rm -rf /tmp/x", "", true},
		{"sudo rm /x", "no_sudo", false},
		{"ls", "", false},
	}
	for i, c := range cases {
		o := outcomes[i]
		if o.Rule != "no_rm" || o.Kind != ExampleKindDeny || o.Example != c.ex || o.Pass != c.pass || o.Matched != c.matched {
			t.Errorf("example %q: got %+v, want pass=%t matched=%q", c.ex, o, c.pass, c.matched)
		}
	}
}

// TestRunExamplesAllow is the allow contract: an example passes if the own
// rule is not the matching rule — another rule matching is fine and is
// noted, the own rule matching is a failure naming it.
func TestRunExamplesAllow(t *testing.T) {
	rules := []Rule{
		{Name: "no_sudo", Tool: "Bash", Action: ActionDeny, Command: Patterns{"sudo( |$)"}, Reason: "r"},
		{Name: "no_rm", Tool: "Bash", Action: ActionDeny, Command: Patterns{"rm( |$)"}, Reason: "r",
			AllowExamples: Examples{{Text: "trash /tmp/x"}, {Text: "sudo rm /x"}, {Text: "rm -rf /tmp/x"}}},
	}
	outcomes := RunExamples(rules, WhenRun)
	if len(outcomes) != 3 {
		t.Fatalf("outcomes = %+v, want 3", outcomes)
	}
	cases := []struct {
		ex, matched string
		pass        bool
	}{
		{"trash /tmp/x", "", true},
		{"sudo rm /x", "no_sudo", true},
		{"rm -rf /tmp/x", "no_rm", false},
	}
	for i, c := range cases {
		o := outcomes[i]
		if o.Rule != "no_rm" || o.Kind != ExampleKindAllow || o.Example != c.ex || o.Pass != c.pass || o.Matched != c.matched {
			t.Errorf("example %q: got %+v, want pass=%t matched=%q", c.ex, o, c.pass, c.matched)
		}
	}
}

// TestRunExamplesOrder proves outcomes come in set order, each rule's deny
// examples before its allow examples, so reports are deterministic.
func TestRunExamplesOrder(t *testing.T) {
	rules := []Rule{
		{Name: "a", Tool: "Bash", Action: ActionDeny, Command: Patterns{"rm( |$)"}, Reason: "r",
			AllowExamples: Examples{{Text: "trash x"}}, DenyExamples: Examples{{Text: "rm x"}}},
		{Name: "b", Tool: "Read", Action: ActionDeny, Input: map[string]Patterns{"file_path": {`^/etc/`}}, Reason: "r",
			DenyExamples: Examples{{Text: `{"file_path":"/etc/passwd"}`}}},
	}
	var got []string
	for _, o := range RunExamples(rules, WhenRun) {
		got = append(got, o.Rule+"/"+o.Kind)
	}
	want := []string{"a/deny", "a/allow", "b/deny"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// TestRunExamplesDisabledRuleNotRun proves examples of a rule that is not
// in the resolved set never run: disabling the rule in a later layer drops
// it (and its examples) from Resolve's output.
func TestRunExamplesDisabledRuleNotRun(t *testing.T) {
	base := []Rule{{Name: "no_rm", Tool: "Bash", Action: ActionDeny, Command: Patterns{"rm( |$)"}, Reason: "r",
		DenyExamples: Examples{{Text: "rm x"}}}}
	resolved, _ := Resolve(base, []Rule{{Name: "no_rm", Enabled: boolp(false)}})
	if got := names(resolved); len(got) != 0 {
		t.Fatalf("resolved = %v, want empty", got)
	}
	if outcomes := RunExamples(resolved, WhenRun); len(outcomes) != 0 {
		t.Errorf("outcomes = %+v, want none", outcomes)
	}
}

// TestRunExamplesNonBashInput proves a non-Bash example is fed to Match as
// the tool_input object itself.
func TestRunExamplesNonBashInput(t *testing.T) {
	rules := []Rule{{Name: "no_etc_read", Tool: "Read", Action: ActionDeny, Input: map[string]Patterns{"file_path": {`^/etc/`}}, Reason: "r",
		DenyExamples:  Examples{{Text: `{"file_path":"/etc/passwd"}`}},
		AllowExamples: Examples{{Text: `{"file_path":"/home/x"}`}}}}
	outcomes := RunExamples(rules, WhenRun)
	if len(outcomes) != 2 || !outcomes[0].Pass || !outcomes[1].Pass {
		t.Errorf("outcomes = %+v, want both passing", outcomes)
	}
}

// TestRunExamplesCwd proves the table form's cwd reaches the match: a cwd
// pattern only holds for the example's directory.
func TestRunExamplesCwd(t *testing.T) {
	rules := []Rule{{Name: "no_rm_home", Tool: "Bash", Action: ActionDeny, Command: Patterns{"rm( |$)"}, Cwd: Patterns{`^/home$`}, Reason: "r",
		DenyExamples: Examples{{Text: "rm x", Cwd: "/home", table: true}, {Text: "rm x", Cwd: "/tmp", table: true}}}}
	outcomes := RunExamples(rules, WhenRun)
	if outcomes[0].Pass != true || outcomes[1].Pass != false {
		t.Errorf("outcomes = %+v, want cwd /home to pass and /tmp to fail", outcomes)
	}
}

// TestRunExamplesWhenRunsForReal proves WhenRun executes when-scripts: the
// deny example of a when rule flips once the script's marker exists.
func TestRunExamplesWhenRunsForReal(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	rules := []Rule{{Name: "gated", Tool: "Bash", Action: ActionDeny, Command: Patterns{"rm( |$)"}, When: "test -f " + marker, Reason: "r",
		DenyExamples: Examples{{Text: "rm x"}}}}
	if o := RunExamples(rules, WhenRun)[0]; o.Pass {
		t.Errorf("before the marker: %+v, want a failure (when said no)", o)
	}
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if o := RunExamples(rules, WhenRun)[0]; !o.Pass {
		t.Errorf("after the marker: %+v, want a pass (when denied)", o)
	}
}

// TestRunExamplesWhenSkipNeverRunsScripts proves WhenSkip never executes a
// when-script (check-config runs as root) and marks such rules' outcomes
// WhenUnknown, decided on patterns only.
func TestRunExamplesWhenSkipNeverRunsScripts(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	rules := []Rule{{Name: "gated", Tool: "Bash", Action: ActionDeny, Command: Patterns{"rm( |$)"}, When: "touch " + marker + "; exit 0", Reason: "r",
		DenyExamples:  Examples{{Text: "rm x"}, {Text: "ls"}},
		AllowExamples: Examples{{Text: "trash x"}}}}
	for _, o := range RunExamples(rules, WhenSkip) {
		if !o.WhenUnknown {
			t.Errorf("example %q: %+v, want WhenUnknown", o.Example, o)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("when-script ran (marker %s exists)", marker)
		}
	}
	got := RunExamples(rules, WhenSkip)
	if !got[0].Pass || got[1].Pass || !got[2].Pass {
		t.Errorf("pattern verdicts = %+v, want deny-match pass, deny-miss fail, allow-miss pass", got)
	}
}

// TestRunExamplesWhenSkipScansWhenless proves WhenSkip decides a when-less
// rule's examples over the when-less subset of the set: a when-rule that
// would have matched first at runtime can neither steal nor confirm a
// match, because its script must not run.
func TestRunExamplesWhenSkipScansWhenless(t *testing.T) {
	rules := []Rule{
		{Name: "earlier_gated", Tool: "Bash", Action: ActionDeny, Command: Patterns{""}, When: "exit 0", Reason: "r"},
		{Name: "no_rm", Tool: "Bash", Action: ActionDeny, Command: Patterns{"rm( |$)"}, Reason: "r",
			DenyExamples: Examples{{Text: "rm x"}}},
	}
	if res := Match(rules, Call{Tool: "Bash", Input: json.RawMessage(`{"command":"rm x"}`)}); res.Rule == nil || res.Rule.Name != "earlier_gated" {
		t.Fatalf("runtime Match = %+v, want the gated rule first", res.Rule)
	}
	if o := RunExamples(rules, WhenSkip)[0]; !o.Pass || o.WhenUnknown || o.Matched != "" {
		t.Errorf("outcome = %+v, want a pass decided without the gated rule", o)
	}
}

// TestValidateExamplesOnResolvedSet proves the resolved-set pass catches an
// example that only misshapes after the merge: an override layer adds a
// table example to a rule whose resolved tool is not Bash.
func TestValidateExamplesOnResolvedSet(t *testing.T) {
	global := []Rule{{Name: "no_edit", Tool: "Edit", Input: map[string]Patterns{"file_path": {`^/etc/`}}, Reason: "r"}}
	agent := []Rule{{Name: "no_edit", DenyExamples: Examples{{Text: "rm x", table: true}}}}
	resolved, skipped := Resolve(global, agent)
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none (examples never drop a rule from Resolve)", skipped)
	}
	if err := ValidateExamples(resolved); err == nil || !strings.Contains(err.Error(), "no_edit") {
		t.Errorf("err = %v, want a rejection naming no_edit", err)
	}
}
