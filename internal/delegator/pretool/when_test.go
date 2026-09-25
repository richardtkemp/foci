package pretool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func whenRule(name, when string) Rule {
	return Rule{Name: name, Tool: "Read", When: when, Action: ActionDeny, Reason: "r"}
}

// TestWhen_DenyIffExitZero: exit 0 denies, exit 1 is a quiet no, and any
// other outcome fails open and is reported.
func TestWhen_DenyIffExitZero(t *testing.T) {
	c := Call{Tool: "Read", Input: json.RawMessage(`{}`)}
	cases := []struct {
		when    string
		deny    bool
		errWant string
	}{
		{"exit 0", true, ""},
		{"true", true, ""},
		{"exit 1", false, ""},
		{"false", false, ""},
		{"exit 3", false, "exit status 3"},
		{"echo boom >&2; exit 2", false, "boom"},
		{"no-such-command-2034", false, "command not found"},
	}
	for _, tc := range cases {
		res := Match([]Rule{whenRule("w", tc.when)}, c)
		if (res.Rule != nil) != tc.deny {
			t.Errorf("%q: deny = %v, want %v", tc.when, res.Rule != nil, tc.deny)
		}
		switch {
		case tc.errWant == "" && len(res.WhenErrors) > 0:
			t.Errorf("%q: unexpected errors %v", tc.when, res.WhenErrors)
		case tc.errWant != "" && (len(res.WhenErrors) != 1 || !strings.Contains(res.WhenErrors[0].Error(), tc.errWant)):
			t.Errorf("%q: errors = %v, want one containing %q", tc.when, res.WhenErrors, tc.errWant)
		}
	}
}

// TestWhen_RunsOnlyAfterOtherConstraints: a call the patterns reject never
// spawns the check.
func TestWhen_RunsOnlyAfterOtherConstraints(t *testing.T) {
	mark := filepath.Join(t.TempDir(), "ran")
	rules, skipped := Resolve([]Rule{{
		Name:    "w",
		Tool:    "Bash",
		Command: Patterns{`git (\S+ )*commit( |$)`},
		Cwd:     Patterns{`^/`},
		When:    "touch " + mark,
		Reason:  "r",
	}})
	if len(skipped) > 0 {
		t.Fatal(skipped)
	}
	c := bashCall("git status && echo git commit")
	c.Cwd = t.TempDir()
	if r := Match(rules, c).Rule; r != nil {
		t.Fatalf("denied %q", c.Input)
	}
	c = bashCall("git commit -m x")
	if r := Match(rules, c).Rule; r != nil {
		t.Fatal("denied with no cwd (cwd constraint fails)")
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("when ran although the patterns did not match")
	}
	c.Cwd = t.TempDir()
	if r := Match(rules, c).Rule; r == nil {
		t.Fatal("not denied once everything matched")
	}
	if _, err := os.Stat(mark); err != nil {
		t.Fatal("when did not run")
	}
}

// TestWhen_Inputs pins what a check sees: the matched command's words as
// positional parameters (once per matched command), the input as env and
// stdin, and the call's cwd — and no BASH_ENV.
func TestWhen_Inputs(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	bashEnv := filepath.Join(dir, "bashenv")
	if err := os.WriteFile(bashEnv, []byte("echo SOURCED >> "+out+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASH_ENV", bashEnv)

	rules, _ := Resolve([]Rule{{
		Name:    "probe",
		Tool:    "Bash",
		Command: Patterns{`git (\S+ )*checkout`},
		When: `{ printf '[%s]' "$0" "$#" "$@"; echo
		  echo "$TOOL_NAME|$TOOL_INPUT_COMMAND|$TOOL_CWD|$PWD"
		  cat; echo; } >> ` + out + `
		exit 1`,
		Reason: "r",
	}})
	cmd := `echo hi; git checkout -- "a b" && git -C /r checkout -- c`
	c := bashCall(cmd)
	c.Cwd = dir
	res := Match(rules, c)
	if res.Rule != nil || len(res.WhenErrors) > 0 {
		t.Fatalf("res = %+v", res)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	line2 := "Bash|" + cmd + "|" + dir + "|" + dir
	want := "[probe][4][git][checkout][--][a b]\n" + line2 + "\n" + string(c.Input) + "\n" +
		"[probe][6][git][-C][/r][checkout][--][c]\n" + line2 + "\n" + string(c.Input) + "\n"
	if string(got) != want {
		t.Errorf("check saw:\n%s\nwant:\n%s", got, want)
	}
}

// TestWhen_NonStringFieldsAndNames: only string fields become env vars, and
// field names are upper-cased with other characters as _.
func TestWhen_NonStringFieldsAndNames(t *testing.T) {
	rules := []Rule{whenRule("w", `[ "$TOOL_INPUT_FILE_PATH" = /x ] && [ "$TOOL_INPUT_OLD_STRING" = "a b" ] && [ -z "${TOOL_INPUT_LIMIT+set}" ]`)}
	c := Call{Tool: "Read", Input: json.RawMessage(`{"file_path":"/x","old-string":"a b","limit":3}`)}
	if res := Match(rules, c); res.Rule == nil {
		t.Fatalf("res = %+v", res)
	}
}

// TestWhen_TimeoutFailsOpen: a check that hangs, even with a backgrounded
// child holding its stderr, is killed and reported, and does not deny. Once
// the call's budget is spent, later checks are reported without running.
func TestWhen_TimeoutFailsOpen(t *testing.T) {
	oldT, oldB := whenTimeout, whenBudget
	whenTimeout, whenBudget = 200*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { whenTimeout, whenBudget = oldT, oldB })

	mark := filepath.Join(t.TempDir(), "third")
	rules := []Rule{
		whenRule("a", "sleep 30 & sleep 30"),
		whenRule("b", "sleep 30"),
		whenRule("c", "touch "+mark+"; exit 0"),
	}
	res := Match(rules, Call{Tool: "Read", Input: json.RawMessage(`{}`)})
	if res.Rule != nil {
		t.Fatalf("denied by %s", res.Rule.Name)
	}
	if len(res.WhenErrors) != 3 {
		t.Fatalf("errors = %v", res.WhenErrors)
	}
	for i, want := range []string{"rule a: when: timed out", "rule b: when: timed out", "rule c: when: not run"} {
		if !strings.Contains(res.WhenErrors[i].Error(), want) {
			t.Errorf("error %d = %v, want %q", i, res.WhenErrors[i], want)
		}
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("a check ran after the budget was spent")
	}
}

func TestWhen_MissingCwdFailsOpen(t *testing.T) {
	c := Call{Tool: "Read", Input: json.RawMessage(`{}`), Cwd: filepath.Join(t.TempDir(), "gone")}
	res := Match([]Rule{whenRule("w", "exit 0")}, c)
	if res.Rule != nil || len(res.WhenErrors) != 1 || !strings.Contains(res.WhenErrors[0].Error(), "not a directory") {
		t.Errorf("res = %+v", res)
	}
}

// TestWhen_ValidateAndOverlay: a when that doesn't parse is rejected at
// load, and a later layer can add or replace one.
func TestWhen_ValidateAndOverlay(t *testing.T) {
	if err := ValidateLayer([]Rule{{Name: "w", When: "if true; then"}}); err == nil || !strings.Contains(err.Error(), "when") {
		t.Errorf("unparseable when: err = %v", err)
	}
	rules, skipped := Resolve([]Rule{whenRule("w", "exit 1")}, []Rule{{Name: "w", When: "exit 0"}})
	if len(skipped) > 0 || rules[0].When != "exit 0" {
		t.Errorf("rules = %+v, skipped = %v", rules, skipped)
	}
	enc, err := Encode(rules)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := Decode(enc)
	if err != nil || dec[0].When != "exit 0" {
		t.Errorf("wire round trip = %+v, %v", dec, err)
	}
}
