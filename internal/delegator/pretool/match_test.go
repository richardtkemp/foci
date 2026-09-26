package pretool

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func bashCall(cmd string) Call {
	enc, _ := json.Marshal(map[string]string{"command": cmd})
	return Call{Tool: "Bash", Input: enc}
}

func matchName(rules []Rule, c Call) string {
	if r := Match(rules, c).Rule; r != nil {
		return r.Name
	}
	return ""
}

// commandTexts is Parse reduced to each command's Text.
func commandTexts(script string) ([]string, bool) {
	s, ok := Parse(script)
	var out []string
	for _, c := range s.Commands {
		out = append(out, c.Text)
	}
	return out, ok
}

func TestCommands(t *testing.T) {
	cases := []struct {
		script string
		want   []string
	}{
		{"git add -A", []string{"git add -A"}},
		{"cd /r && git add .", []string{"cd /r", "git add ."}},
		{"a; b || c | d\ne", []string{"a", "b", "c", "d", "e"}},
		{"(git -C /x commit -m x)", []string{"git -C /x commit -m x"}},
		{"if true; then git add -A; fi", []string{"true", "git add -A"}},
		{`echo "$(git add -A)"`, []string{"echo $(git␣add␣-A)", "git add -A"}},
		// Quoted whitespace stays inside its word, so a message can't pose as
		// a command.
		{`git commit -m "fix merge; worktree remove"`, []string{"git commit -m fix␣merge;␣worktree␣remove"}},
		{`git commit -m 'a b'`, []string{"git commit -m a␣b"}},
		// Quotes removed, expansions kept, escapes dropped.
		{`git worktree add "$HOME/x" 'main'`, []string{"git worktree add $HOME/x main"}},
		{`echo a\ b "q\"q"`, []string{"echo a␣b q\"q"}},
		// Assignments and redirections, including heredoc bodies, are not words.
		{"X=1 git add -A 2>/dev/null", []string{"git add -A"}},
		{"git commit -F - <<'EOF'\ngit add -A\nEOF", []string{"git commit -F -"}},
	}
	for _, c := range cases {
		got, ok := commandTexts(c.script)
		if !ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("Parse(%q) = %q, %v; want %q", c.script, got, ok, c.want)
		}
	}
	if _, ok := commandTexts("echo 'unterminated"); ok {
		t.Error("unparseable script reported ok")
	}
}

// TestMatch_Command is the Bash-aware matcher's contract: anchored at each
// command's first word, never matched across commands or into quoted text,
// and fails open (no match) on a script that does not parse.
func TestMatch_Command(t *testing.T) {
	rules, skipped := Resolve([]Rule{{
		Name:    "no_add_all",
		Tool:    "Bash",
		Command: Patterns{`git (\S+ )*add (-A|--all)( |$)`},
		Reason:  "r",
	}})
	if len(skipped) > 0 {
		t.Fatal(skipped)
	}
	cases := map[string]string{
		"git add -A":                        "no_add_all",
		"git -C /r add --all && git st":     "no_add_all",
		"cd /r; git add -A":                 "no_add_all",
		"(git add -A)":                      "no_add_all",
		"git add -Aq":                       "",
		"echo git add -A":                   "",
		`git commit -m "then git add -A"`:   "",
		`foci_todo add --text "git add -A"`: "",
		"git add -A 'unterminated":          "",
	}
	for cmd, want := range cases {
		if got := matchName(rules, bashCall(cmd)); got != want {
			t.Errorf("%q: got %q, want %q", cmd, got, want)
		}
	}
}

// TestMatch_AnyOfAndAnd: patterns within one constraint are alternatives;
// separate constraints must all hold.
func TestMatch_AnyOfAndAnd(t *testing.T) {
	rules, _ := Resolve([]Rule{{
		Name:    "gated",
		Tool:    "Bash",
		Command: Patterns{`git (\S+ )*merge( |$)`, `make (\S+ )*land( |$)`},
		Input:   map[string]Patterns{"command": {`(;|\n|\|\|)\s*git\b[^;&|\n]*\bworktree\s+remove`}},
		Reason:  "r",
	}})
	cases := map[string]string{
		"git merge x; git worktree remove /w":     "gated",
		"make -C /w land\ngit worktree remove /w": "gated",
		"git merge x && git worktree remove /w":   "",
		"git fetch; git worktree remove /w":       "",
	}
	for cmd, want := range cases {
		if got := matchName(rules, bashCall(cmd)); got != want {
			t.Errorf("%q: got %q, want %q", cmd, got, want)
		}
	}
}

func TestMatch_Cwd(t *testing.T) {
	rules, _ := Resolve([]Rule{{
		Name:    "no_commit_in_main",
		Tool:    "Bash",
		Command: Patterns{`git (-c \S+ )*commit( |$)`},
		Cwd:     Patterns{`^/home/rich/git/(foci|foci-client)/?$`},
		Reason:  "r",
	}})
	c := bashCall("git commit -m x")
	for cwd, want := range map[string]string{
		"/home/rich/git/foci":        "no_commit_in_main",
		"/home/rich/git/foci-client": "no_commit_in_main",
		"/home/rich/git/foci-wt-1":   "",
		"":                           "",
	} {
		c.Cwd = cwd
		if got := matchName(rules, c); got != want {
			t.Errorf("cwd %q: got %q, want %q", cwd, got, want)
		}
	}
}

// TestPatterns_Decode: a single string (the #2028 syntax) and an array both
// decode, from TOML and from the JSON wire form.
func TestPatterns_Decode(t *testing.T) {
	var cfg struct {
		Rules []Rule `toml:"rules"`
	}
	_, err := toml.Decode(`
[[rules]]
name = "old"
input = { command = "one" }
[[rules]]
name = "new"
input = { command = ["a", "b"] }
command = 'git add'
cwd = ["^/x", "^/y"]
`, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Rules[0].Input["command"]; !reflect.DeepEqual(got, Patterns{"one"}) {
		t.Errorf("old input = %q", got)
	}
	n := cfg.Rules[1]
	if !reflect.DeepEqual(n.Input["command"], Patterns{"a", "b"}) || !reflect.DeepEqual(n.Command, Patterns{"git add"}) ||
		!reflect.DeepEqual(n.Cwd, Patterns{"^/x", "^/y"}) {
		t.Errorf("new rule = %+v", n)
	}
	if _, err := toml.Decode("[[rules]]\nname = \"x\"\ncommand = 3\n", &cfg); err == nil || !strings.Contains(err.Error(), "regex") {
		t.Errorf("non-string pattern: err = %v", err)
	}

	var r Rule
	if err := json.Unmarshal([]byte(`{"name":"j","input":{"command":"one"},"cwd":["a","b"]}`), &r); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Input["command"], Patterns{"one"}) || !reflect.DeepEqual(r.Cwd, Patterns{"a", "b"}) {
		t.Errorf("json rule = %+v", r)
	}
}

// TestResolve_OverrideCommandAndCwd: a later layer replaces command and cwd
// like any other field, and a command rule must end up on Bash.
func TestResolve_OverrideCommandAndCwd(t *testing.T) {
	base := []Rule{{Name: "r", Tool: "Bash", Command: Patterns{"a"}, Reason: "x"}}
	rules, _ := Resolve(base, []Rule{{Name: "r", Command: Patterns{"b"}, Cwd: Patterns{"c"}}})
	if !reflect.DeepEqual(rules[0].Command, Patterns{"b"}) || !reflect.DeepEqual(rules[0].Cwd, Patterns{"c"}) {
		t.Errorf("rule = %+v", rules[0])
	}
	_, skipped := Resolve(base, []Rule{{Name: "r", Tool: "Read"}})
	if len(skipped) != 1 || !strings.Contains(skipped[0], "command applies only") {
		t.Errorf("skipped = %v", skipped)
	}
}
