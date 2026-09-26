package pretool

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// facts renders the structural facts of each command in script, one line per
// command, for compact comparison: text, then the flags that are set, dir
// (? = unknown), the operator before it and its downstream pipe.
func facts(t *testing.T, script string) []string {
	t.Helper()
	s, ok := Parse(script)
	if !ok {
		t.Fatalf("Parse(%q) failed", script)
	}
	var out []string
	for _, c := range s.Commands {
		line := c.Text
		if c.Background {
			line += " bg"
		}
		if c.Subshell {
			line += " sub"
		}
		if c.Output {
			line += " out"
		}
		if !c.DirKnown {
			line += " dir=?"
		} else if c.Dir != "" {
			line += " dir=" + c.Dir
		}
		if c.Op != "" {
			line += " op=" + c.Op
		}
		if len(c.Pipe) > 0 {
			line += " pipe=" + strings.Join(c.Pipe, ",")
		}
		out = append(out, line)
	}
	return out
}

// TestParse_Facts pins the structural facts the parser derives for each
// simple command (#2040): what rules match on instead of the raw text.
func TestParse_Facts(t *testing.T) {
	t.Setenv("HOME", "/h")
	cases := []struct {
		script string
		want   []string
	}{
		// Heredoc bodies and quoted text are not commands and carry no
		// operators; the command that owns them is still output.
		{"python3 - <<'EOF'\nx = '&r'\nok &= good\ncd /tmp\nEOF", []string{"python3 - out"}},
		{"cat <<EOF\na & b\nEOF", []string{"cat out"}},
		{`git commit -m "R&D; cd /x & y"`, []string{"git commit -m R&D;␣cd␣/x␣&␣y out"}},
		// Background: the statement and everything in it.
		{"sleep 5 &", []string{"sleep 5 bg sub out"}},
		{"a & b", []string{"a bg sub out", "b out op=&"}},
		{"(a; b) &", []string{"a bg sub out", "b bg sub out op=;"}},
		{`echo "$(sleep 1 &)"`, []string{"echo $(sleep␣1␣&) out", "sleep 1 bg sub"}},
		{"python3 - <<'EOF' &\nx\nEOF", []string{"python3 - bg sub out"}},
		// Not background: redirects and pipes that contain &.
		{"a &>/dev/null; b 2>&1 |& c", []string{"a", "b sub op=; pipe=c", "c sub out op=|&"}},
		// Subshells: ( ), $( ), <( ), pipeline elements. { } and if bodies
		// run in the main shell.
		{"(cd /x && ls)", []string{"cd /x sub out", "ls sub out dir=/x op=&&"}},
		{"x=$(cd /x; pwd)", []string{"cd /x sub", "pwd sub dir=/x op=;"}},
		{"{ cd /x; ls; }", []string{"cd /x out", "ls out dir=/x op=;"}},
		{"if true; then cd /x; else cd /y; fi", []string{"true out", "cd /x out op=&&", "cd /y out dir=/x op=||"}},
		{"diff <(sort a) b", []string{"diff <(sort␣a) b out", "sort a sub"}},
		// Directory tracking: relative to the call's cwd ("") until a cd,
		// ~ and $HOME resolved, anything dynamic unknown, and a subshell's
		// cd does not leak out.
		{"cd sub && ls; cd ..; pwd", []string{"cd sub out", "ls out dir=sub op=&&", "cd .. out dir=sub op=;", "pwd out op=;"}},
		{"cd; a; cd ~/x; b; cd \"$HOME/y\"; c", []string{"cd out", "a out dir=/h op=;", "cd ~/x out dir=/h op=;", "b out dir=/h/x op=;", "cd $HOME/y out dir=/h/x op=;", "c out dir=/h/y op=;"}},
		{`cd "$W"; a; cd /x; b`, []string{"cd $W out", "a out dir=? op=;", "cd /x out dir=? op=;", "b out dir=/x op=;"}},
		{"cd -- /x; a; cd -; b", []string{"cd -- /x out", "a out dir=/x op=;", "cd - out dir=/x op=;", "b out dir=? op=;"}},
		{"(cd /x); a", []string{"cd /x sub out", "a out op=;"}},
		{"pushd /x; a; popd; b", []string{"pushd /x out", "a out dir=/x op=;", "popd out dir=/x op=;", "b out dir=? op=;"}},
		// Output: stdout redirected, piped or captured is not output; a
		// redirect on an enclosing group applies to what is inside it.
		{"cat f > x; cat f >> x; cat f >&2; cat f 2>/dev/null", []string{"cat f", "cat f op=;", "cat f out op=;", "cat f out op=;"}},
		{"{ cat f; } > x; for f in a; do cat $f; done | wc -l", []string{"cat f", "cat $f sub op=; pipe=wc -l", "wc -l sub out op=|"}},
		// Pipe: every downstream command of the pipeline.
		{"make 2>&1 | tee l | tail -5", []string{"make sub pipe=tee l,tail -5", "tee l sub op=| pipe=tail -5", "tail -5 sub out op=|"}},
	}
	for _, c := range cases {
		if got := facts(t, c.script); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Parse(%q):\n got  %q\n want %q", c.script, got, c.want)
		}
	}
}

func TestParse_EndDir(t *testing.T) {
	t.Setenv("HOME", "/h")
	for script, want := range map[string]string{
		"ls":                         "",
		"cd /x && ls":                "/x",
		"(cd /x && ls)":              "",
		"cd /x; (cd /y); ls":         "/x",
		"cd /x | cat":                "",
		"cd /x &":                    "",
		`cd "$W"`:                    "?",
		"cd /x\ncat <<'E'\ncd /y\nE": "/x",
	} {
		s, _ := Parse(script)
		got := s.EndDir
		if !s.EndDirKnown {
			got = "?"
		}
		if got != want {
			t.Errorf("%q: end dir %q, want %q", script, got, want)
		}
	}
}

// TestMatch_FactFilters: background / subshell / output narrow the commands a
// rule applies to, alone or with command patterns.
func TestMatch_FactFilters(t *testing.T) {
	yes, no := true, false
	rules, skipped := Resolve([]Rule{
		{Name: "bg", Tool: "Bash", Background: &yes, Reason: "r"},
		{Name: "cd", Tool: "Bash", Command: Patterns{`cd( |$)`}, Subshell: &no, Reason: "r"},
		{Name: "cat", Tool: "Bash", Command: Patterns{`cat( |$)`}, Output: &yes, Reason: "r"},
	})
	if len(skipped) > 0 {
		t.Fatal(skipped)
	}
	cases := map[string]string{
		"python3 - <<'EOF'\nok &= good\nEOF": "",
		"sleep 5 &":                          "bg",
		`echo "$(sleep 1 &)"`:                "bg",
		"cd /x && ls":                        "cd",
		"{ cd /x; }":                         "cd",
		"(cd /x && ls)":                      "",
		"cat > f <<'EOF'\ncd /x\nEOF":        "",
		"cat f":                              "cat",
		"cat f | head":                       "",
		"x=$(cat f)":                         "",
		"echo cat f":                         "",
	}
	for cmd, want := range cases {
		if got := matchName(rules, bashCall(cmd)); got != want {
			t.Errorf("%q: got %q, want %q", cmd, got, want)
		}
	}
}

// TestFilters_ValidateOverlayWire: filters are Bash-only, override like any
// field, and survive the hook's wire encoding.
func TestFilters_ValidateOverlayWire(t *testing.T) {
	yes, no := true, false
	if err := ValidateLayer([]Rule{{Name: "x", Tool: "Read", Background: &yes}}); err == nil || !strings.Contains(err.Error(), "background") {
		t.Errorf("filter on Read: err = %v", err)
	}
	rules, skipped := Resolve(
		[]Rule{{Name: "r", Tool: "Bash", Background: &yes, Reason: "x"}},
		[]Rule{{Name: "r", Background: &no, Subshell: &no, Output: &yes}},
	)
	if len(skipped) > 0 {
		t.Fatal(skipped)
	}
	enc, err := Encode(rules)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	r := dec[0]
	if r.Background == nil || *r.Background || r.Subshell == nil || *r.Subshell || r.Output == nil || !*r.Output {
		t.Errorf("wire rule = %+v", r)
	}
}

// TestWhen_CommandFacts: a when-check gets the matched command's facts
// (CMD_DIR, CMD_OP, CMD_PIPE) and the call's (TOOL_COMMANDS, TOOL_END_DIR),
// and inherited values of those names are dropped.
func TestWhen_CommandFacts(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	t.Setenv("CMD_DIR", "stale")
	t.Setenv("TOOL_END_DIR", "stale")
	rules, _ := Resolve([]Rule{{
		Name:    "probe",
		Tool:    "Bash",
		Command: Patterns{`make( |$)`},
		When: `printf '%s|%s|%s|%s|%s\n' "$CMD_DIR" "$CMD_OP" "$CMD_PIPE" "$TOOL_END_DIR" "$TOOL_COMMANDS" >> ` + out + `
		exit 1`,
		Reason: "r",
	}})
	c := bashCall("cd sub && make x | tail -1; (cd /abs; make y); cd \"$W\"; make z")
	c.Cwd = dir
	if res := Match(rules, c); res.Rule != nil || len(res.WhenErrors) > 0 {
		t.Fatalf("res = %+v", res)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	all := "cd sub\nmake x\ntail -1\ncd /abs\nmake y\ncd $W\nmake z"
	want := filepath.Join(dir, "sub") + "|&&|tail -1||" + all + "\n" +
		"/abs|;|||" + all + "\n" +
		"|;|||" + all + "\n"
	if string(got) != want {
		t.Errorf("check saw:\n%s\nwant:\n%s", got, want)
	}
}
