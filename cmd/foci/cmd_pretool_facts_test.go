package main

import (
	"bytes"
	"strings"
	"testing"
)

// pretoolFactRules are clutch's rules that used to regex the raw command
// text in a when-check, rewritten on the parsed script's facts (#2040):
// background & and a main-shell cd are properties of commands, so text in a
// heredoc body or a quoted argument can no longer trip them.
const pretoolFactRules = `
[[agents]]
id = "clutch"
backend = "claude-code"

[[agents.backend_config.pretool_rules]]
name = "no_trailing_amp"
tool = "Bash"
background = true
reason = "Do not background with &."

[[agents.backend_config.pretool_rules]]
name = "no_persisted_cd"
tool = "Bash"
command = '(cd|pushd)( |$)'
subshell = false
when = '''
shift
t=
for a in "$@"; do case $a in --|-[!-]*) ;; *) t=$a ;; esac; done
case $t in ''|'~'|'~/'|'$HOME'|'$HOME/'|/home/foci/clutch|/home/foci/clutch/) exit 1 ;; esac
exit 0
'''
reason = "Do not cd in the main shell."

[[agents.backend_config.pretool_rules]]
name = "worktree_remove_gated"
tool = "Bash"
command = 'git (\S+ )*worktree remove( |$)'
when = '''
case $CMD_OP in ';'|'||'|'&') ;; *) exit 1 ;; esac
printf '%s\n' "$TOOL_COMMANDS" | grep -Eq '^(sudo (\S+ )*)?(git (\S+ )*merge|make (\S+ )*land)( |$)'
'''
reason = "Chain the cleanup on the merge with &&."

[[agents.backend_config.pretool_rules]]
name = "no_piped_build"
tool = "Bash"
command = ['(sudo (\S+ )*)?make( |$)', 'go (\S+ )*test( |$)']
when = '''
printf '%s\n' "$CMD_PIPE" | grep -Eq '^(tail|head|grep)( |$)'
'''
reason = "Do not pipe make or go test into tail, head or grep."
`

// TestPretoolTest_FactRules: the cases the raw-text rules got wrong
// (heredoc and quoted text denied; { cd; }, cd "x" and & in $( ) allowed),
// and the ones they already got right, through ` + "`foci pretool test`" + `.
func TestPretoolTest_FactRules(t *testing.T) {
	path := writePretoolConfig(t, pretoolFactRules)
	cases := []struct{ cmd, want string }{
		// Reported repros: & in a quoted heredoc body.
		{"python3 - <<'EOF'\nx = '&r'\nEOF", ""},
		{"python3 - <<'EOF'\nok &= good\nEOF", ""},
		{"cat <<EOF\na & b\nEOF", ""},
		{`echo a\&b`, ""},
		{`git commit -m "R&D fixes & more"`, ""},
		{"foo &>/dev/null; ls |& grep x", ""},
		{"sleep 5 &", "no_trailing_amp"},
		{"(sleep 5 &)", "no_trailing_amp"},
		{`echo "$(sleep 100 &)"`, "no_trailing_amp"},
		{"python3 - <<'EOF' &\nprint(1)\nEOF", "no_trailing_amp"},
		// cd: a command in the main shell, not text.
		{"cd /tmp && ls", "no_persisted_cd"},
		{"ls\ncd /tmp", "no_persisted_cd"},
		{"{ cd /tmp; ls; }", "no_persisted_cd"},
		{"if true; then cd /tmp; fi", "no_persisted_cd"},
		{`cd "/tmp" && ls`, "no_persisted_cd"},
		{`cd "$W" && ls`, "no_persisted_cd"},
		{"cd - ", "no_persisted_cd"},
		{"(cd /tmp && ls)", ""},
		{"x=$(cd /tmp && pwd)", ""},
		{"cat > /tmp/x.sh <<'EOF'\ncd /tmp\nEOF", ""},
		{`git commit -m "then cd /tmp"`, ""},
		{"cd /home/foci/clutch && ls", ""},
		{"cd; cd ~; cd $HOME", ""},
		// Separators between real commands only.
		{"git -C /r merge x; git -C /r worktree remove /w", "worktree_remove_gated"},
		{"make -C /r land\ngit -C /r worktree remove /w", "worktree_remove_gated"},
		{"git -C /r merge x && git -C /r worktree remove /w", ""},
		{`git -C /r merge x && git commit -m "ok; git worktree remove later"`, ""},
		{"git -C /r merge x && cat <<'EOF'\n; git worktree remove /w\nEOF", ""},
		// A pipe into tail/head/grep, anywhere downstream.
		{"make -C /r test 2>&1 | tail -5", "no_piped_build"},
		{"make -C /r test 2>&1 | tee /tmp/l | grep FAIL", "no_piped_build"},
		{"make -C /r test > /tmp/l 2>&1; tail /tmp/l", ""},
		{"make -C /r test > /tmp/l 2>&1; cat <<'EOF'\nmake | tail\nEOF", ""},
	}
	for _, c := range cases {
		var out bytes.Buffer
		if err := cmdPretool([]string{"--config", path, "--agent", "clutch", "test", "--bash", c.cmd, "--cwd", t.TempDir()}, &out); err != nil {
			t.Fatalf("%q: %v", c.cmd, err)
		}
		want := c.want
		if want == "" {
			want = "no match"
		}
		if got := strings.TrimSpace(out.String()); got != want {
			t.Errorf("%q: got %q, want %q", c.cmd, got, want)
		}
	}
}

// TestPretoolTest_VerboseFacts: -v shows each command's facts, so a rule
// author can see what background/subshell/output and the CMD_* inputs will be.
func TestPretoolTest_VerboseFacts(t *testing.T) {
	path := writePretoolConfig(t, pretoolFactRules)
	var out bytes.Buffer
	err := cmdPretool([]string{"--config", path, "--agent", "clutch", "test", "-v", "--bash",
		"(cd /x && make -C y 2>&1 | tail -3) &", "--cwd", t.TempDir()}, &out)
	if err != nil {
		t.Fatal(err)
	}
	want := "no_trailing_amp\n" +
		"  command: cd /x  [background, subshell, output]\n" +
		"  command: make -C y  [background, subshell, dir=/x, op=&&, pipe=tail -3]\n" +
		"  command: tail -3  [background, subshell, output, dir=/x, op=|]\n" +
		"  reason: Do not background with &.\n"
	if got := out.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
