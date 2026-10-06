package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The five trial rules on clutch's agent (#2029 candidates 3 4 7 8 9), in
// the #2028 syntax as first configured: one raw regex over the whole command,
// needing a statement-start prefix and \x22/\x27 escapes to keep out of
// quoted text. Kept to prove the old syntax still loads and classifies.
const pretoolTrialOldSyntax = `
[[agents]]
id = "clutch"
backend = "claude-code"
backend_config.pretool_rules = [
  { name = "worktree_abs_path", tool = "Bash", input = { command = '(^|[;&|\n(])\s*git\b[^;&|\n\x22\x27\x60]*\bworktree\s+add((\s+-[bB]\s+\S+)|(\s+--[\w=-]+)|(\s+-[ac-zAC-Z]\w*))*\s+[^-/~$\s\x22\x27]' }, reason = "git worktree add needs an ABSOLUTE path. A relative path resolves against -C <repo> and nests the worktree inside the repo. Use /home/rich/git/<repo>-wt-<name>." },
  { name = "worktree_base", tool = "Bash", input = { command = '(^|[;&|\n(])\s*git\b[^;&|\n\x22\x27\x60]*\bworktree\s+add\b[^;&|\n]*\s(main|master)\s*($|[;&|\n])|(^|[;&|\n(])\s*git\b[^;&|\n\x22\x27\x60]*\bworktree\s+add((\s+-[bB]\s+\S+)|(\s+--[\w=-]+)|(\s+-[ac-zAC-Z]\w*))*\s+[^-\s;&|]\S*\s*($|[;&|\n])' }, reason = "Cut a worktree from origin/main after a fetch: git -C <repo> fetch, then git -c core.sharedRepository=false -C <repo> worktree add -b <branch> <abs-path> origin/main. Local main, or no base (current HEAD), can be stale." },
  { name = "worktree_remove_gated", tool = "Bash", input = { command = '(^|[;&|\n(])\s*(git\b[^;&|\n]*\bmerge\b|make\b[^;&|\n]*\bland\b)[\s\S]*(;|\n|\|\|)\s*git\b[^;&|\n]*\bworktree\s+remove' }, reason = "Gate worktree cleanup on the merge: chain it with && (merge ... && git worktree remove ...), not with ; or || or a new line. If the land already succeeded, run the remove as its own command." },
  { name = "no_git_add_all", tool = "Bash", input = { command = '(^|[;&|\n(])\s*git\b[^;&|\n\x22\x27\x60]*\sadd\s+(-A|--all|\.)(\s|$|[;&|])' }, reason = "Do not stage with git add -A, --all or '.': in shared repos it sweeps in other agents' work and stale files. Stage explicit paths." },
  { name = "no_commit_main_checkout", tool = "Bash", input = { command = '(^|[;&|\n(])\s*git\s+(-c\s+\S+\s+)*-C\s+/home/rich/git/(foci|foci-client)/?\s[^;&|\n]*\bcommit\b' }, reason = "Do not commit in the foci or foci-client main checkout. Work in a worktree on a feature branch and land it with make -C <worktree> land." },
]
`

// The same five rules in the #2033 syntax (#2033 acceptance). command
// patterns are matched against each simple command separately, from its
// first word, with quoted whitespace shown as a non-space, so none of them
// needs to guard against text inside quotes or neighbouring statements.
const pretoolTrialNewSyntax = `
[[agents]]
id = "clutch"
backend = "claude-code"

[[agents.backend_config.pretool_rules]]
name = "worktree_abs_path"
tool = "Bash"
# The path is the first word after the options; -b/-B take a value.
command = 'git (\S+ )*worktree add( -[bB] \S+| --\S+| -[^bB ]\S*)* [^-/~$]'
reason = "git worktree add needs an ABSOLUTE path. A relative path resolves against -C <repo> and nests the worktree inside the repo. Use /home/rich/git/<repo>-wt-<name>."

[[agents.backend_config.pretool_rules]]
name = "worktree_base"
tool = "Bash"
command = [
  'git (\S+ )*worktree add (\S+ )*(main|master)$',                     # based on local main
  'git (\S+ )*worktree add( -[bB] \S+| --\S+| -[^bB ]\S*)* \S+$',      # no base: path only
]
reason = "Cut a worktree from origin/main after a fetch: git -C <repo> fetch, then git -c core.sharedRepository=false -C <repo> worktree add -b <branch> <abs-path> origin/main. Local main, or no base (current HEAD), can be stale."

[[agents.backend_config.pretool_rules]]
name = "worktree_remove_gated"
tool = "Bash"
# A merge or land happens, and a worktree remove follows ; || or a newline.
command = ['git (\S+ )*merge( |$)', 'make (\S+ )*land( |$)']
input.command = '(;|\n|\|\|)\s*git\b[^;&|\n]*\bworktree\s+remove'
reason = "Gate worktree cleanup on the merge: chain it with && (merge ... && git worktree remove ...), not with ; or || or a new line. If the land already succeeded, run the remove as its own command."

[[agents.backend_config.pretool_rules]]
name = "no_git_add_all"
tool = "Bash"
command = 'git (\S+ )*add (\S+ )*(-A|--all|\.)( |$)'
reason = "Do not stage with git add -A, --all or '.': in shared repos it sweeps in other agents' work and stale files. Stage explicit paths."

[[agents.backend_config.pretool_rules]]
name = "no_commit_main_checkout"
tool = "Bash"
command = 'git (-c \S+ )*-C /home/rich/git/(foci|foci-client)/? (-c \S+ )*commit( |$)'
reason = "Do not commit in the foci or foci-client main checkout. Work in a worktree on a feature branch and land it with make -C <worktree> land."
`

// pretoolTrialCases are the 30 commands the trial rules were tuned on
// (/tmp/pretool-try/c2.tsv). Empty want = no rule denies it.
var pretoolTrialCases = []struct{ cmd, want string }{
	{"git -c core.sharedRepository=false -C /home/rich/git/foci worktree add -b feat wt-feat origin/main", "worktree_abs_path"},
	{"git worktree add ../foci-wt-x origin/main", "worktree_abs_path"},
	{"git -c core.sharedRepository=false -C /home/rich/git/foci worktree add -q -b feat /home/rich/git/foci-wt-feat origin/main", ""},
	{"git worktree add -b feat $W origin/main", ""},
	{"git -c core.sharedRepository=false -C /home/rich/git/foci worktree add -b feat /home/rich/git/foci-wt-feat main", "worktree_base"},
	{"git -c core.sharedRepository=false -C /home/rich/git/foci worktree add -b feat /home/rich/git/foci-wt-feat", "worktree_base"},
	{"git -C /r worktree add -b feat /r-wt && cd /r-wt", "worktree_base"},
	{"git -C /r worktree add /r-wt existing-branch", ""},
	{"git -C /r worktree list", ""},
	{"git -C /r merge --ff-only feat; git -C /r worktree remove /r-wt", "worktree_remove_gated"},
	{"git -C /r merge --ff-only feat && git -C /r worktree remove /r-wt", ""},
	{"git -C /r worktree remove --force /r-wt", ""},
	{"git add -A", "no_git_add_all"},
	{"git -C /home/foci add .", "no_git_add_all"},
	{"git -C /r add --all && git commit -m x", "no_git_add_all"},
	{"git -C /r add ./foo.go bar.go", ""},
	{"git -C /r add memory/x.md", ""},
	{"git -C /home/rich/git/foci commit -m x", "no_commit_main_checkout"},
	{"git -c core.x=y -C /home/rich/git/foci-client/ commit -qm x", "no_commit_main_checkout"},
	{"git -C /home/rich/git/foci-wt-2028 commit -m x", ""},
	{"git -C /home/rich/git/foci log --oneline -1", ""},
	{"git commit -m \"fix merge; worktree remove later\"", ""},
	{"git -C /r merge --ff-only feat\ngit -C /r worktree remove /r-wt", "worktree_remove_gated"},
	{"foci_todo add --text \"never git add -A in shared repos\"", ""},
	{"git -C /r commit -m \"doc: git worktree add -b x rel main is bad\"", ""},
	{"echo git -C /home/rich/git/foci commit", ""},
	{"git -C /r worktree prune", ""},
	{"git worktree add -b feat /abs/wt origin/main && cd /abs/wt", ""},
	{"cd /r && git add .", "no_git_add_all"},
	{"(git -C /home/rich/git/foci commit -m x)", "no_commit_main_checkout"},
}

func writePretoolConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foci.toml")
	cfg := "[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n" + body
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPretoolTest_TrialRules runs every trial case through ` + "`foci pretool test`" + `
// against a config file, in both syntaxes.
func TestPretoolTest_TrialRules(t *testing.T) {
	for label, body := range map[string]string{
		"old syntax": pretoolTrialOldSyntax,
		"new syntax": pretoolTrialNewSyntax,
	} {
		t.Run(label, func(t *testing.T) {
			path := writePretoolConfig(t, body)
			for _, c := range pretoolTrialCases {
				var out bytes.Buffer
				err := cmdPretool([]string{"--config", path, "--agent", "clutch", "test", "--bash", c.cmd}, &out)
				if err != nil {
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
		})
	}
}

// pretoolExamplesConfig is a mixed example set for `foci pretool test --all`
// (#2039): one deny example passes, one is stolen by an earlier rule, one
// matches nothing, and a non-Bash rule's JSON examples both pass.
const pretoolExamplesConfig = `
[[agents]]
id = "clutch"
backend = "claude-code"

[[agents.backend_config.pretool_rules]]
name = "no_sudo"
tool = "Bash"
command = 'sudo( |$)'
reason = "r"

[[agents.backend_config.pretool_rules]]
name = "no_rm"
tool = "Bash"
command = '(sudo (\S+ )*)?rm( |$)'
deny_examples  = ["rm -rf /tmp/x", "sudo rm /x", "ls"]
allow_examples = ["trash /tmp/x"]
reason = "r"

[[agents.backend_config.pretool_rules]]
name = "no_etc_read"
tool = "Read"
input.file_path = '^/etc/'
deny_examples  = ['{"file_path":"/etc/passwd"}']
allow_examples = ['{"file_path":"/home/x"}']
reason = "r"
`

// TestPretoolTest_AllFailures proves --all prints exactly one line per
// failed example — naming the rule, the example, the expected rule and the
// rule that matched instead (or nothing) — plus a summary, and returns a
// non-nil error so the process exits non-zero.
func TestPretoolTest_AllFailures(t *testing.T) {
	path := writePretoolConfig(t, pretoolExamplesConfig)
	var out bytes.Buffer
	err := cmdPretool([]string{"--config", path, "--agent", "clutch", "test", "--all"}, &out)
	if err == nil || !strings.Contains(err.Error(), "2 of 6") {
		t.Fatalf("err = %v, want 2 of 6 examples failed", err)
	}
	got := out.String()
	wantLines := []string{
		"FAIL no_rm deny sudo rm /x: expected no_rm, matched no_sudo",
		"FAIL no_rm deny ls: expected no_rm, matched nothing",
		"2 of 6 pretool example(s) failed",
	}
	for _, w := range wantLines {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q:\n%s", w, got)
		}
	}
	if n := strings.Count(got, "FAIL "); n != 2 {
		t.Errorf("%d FAIL lines, want 2:\n%s", n, got)
	}
}

// TestPretoolTest_AllPasses proves passing examples are silent without -v
// (summary only, no error) and that -v prints one ok line each, noting the
// rule that legally matched an allow example.
func TestPretoolTest_AllPasses(t *testing.T) {
	path := writePretoolConfig(t, `
[[agents]]
id = "clutch"
backend = "claude-code"

[[agents.backend_config.pretool_rules]]
name = "no_sudo"
tool = "Bash"
command = 'sudo( |$)'
reason = "r"

[[agents.backend_config.pretool_rules]]
name = "no_rm"
tool = "Bash"
command = 'rm( |$)'
deny_examples  = ["rm -rf /tmp/x"]
allow_examples = ["sudo rm /x", "trash /tmp/x"]
reason = "r"
`)
	var out bytes.Buffer
	if err := cmdPretool([]string{"--config", path, "--agent", "clutch", "test", "--all"}, &out); err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := out.String(); got != "0 of 3 pretool example(s) failed\n" {
		t.Errorf("quiet output = %q", got)
	}
	out.Reset()
	if err := cmdPretool([]string{"--config", path, "--agent", "clutch", "test", "--all", "-v"}, &out); err != nil {
		t.Fatalf("-v err = %v", err)
	}
	got := out.String()
	for _, w := range []string{
		"ok no_rm deny rm -rf /tmp/x",
		"ok no_rm allow sudo rm /x (matched no_sudo)",
		"ok no_rm allow trash /tmp/x",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("-v output missing %q:\n%s", w, got)
		}
	}
}

// TestPretoolTest_AllRunsWhenScripts proves --all evaluates when-checks for
// real: a gated rule's deny example fails until the when script's marker
// exists, then passes.
func TestPretoolTest_AllRunsWhenScripts(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	path := writePretoolConfig(t, `
[[agents]]
id = "clutch"
backend = "claude-code"

[[agents.backend_config.pretool_rules]]
name = "gated_rm"
tool = "Bash"
command = 'rm( |$)'
when = 'test -f `+marker+`'
deny_examples = ["rm x"]
reason = "r"
`)
	var out bytes.Buffer
	if err := cmdPretool([]string{"--config", path, "--agent", "clutch", "test", "--all"}, &out); err == nil {
		t.Fatalf("before the marker: err = nil, output:\n%s", out.String())
	}
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := cmdPretool([]string{"--config", path, "--agent", "clutch", "test", "--all"}, &out); err != nil {
		t.Fatalf("after the marker: %v, output:\n%s", err, out.String())
	}
}

// TestPretoolTest_AllFlagExclusivity proves --all rejects the single-call
// flags it cannot be combined with.
func TestPretoolTest_AllFlagExclusivity(t *testing.T) {
	path := writePretoolConfig(t, pretoolExamplesConfig)
	for _, flags := range [][]string{
		{"--bash", "rm x"},
		{"--tool", "Bash"},
		{"--input", "{}"},
		{"--cwd", "/tmp"},
	} {
		args := append([]string{"--config", path, "--agent", "clutch", "test", "--all"}, flags...)
		var out bytes.Buffer
		err := cmdPretool(args, &out)
		if err == nil || !strings.Contains(err.Error(), "--all") {
			t.Errorf("%v: err = %v, want a --all exclusivity error", flags, err)
		}
	}
}
