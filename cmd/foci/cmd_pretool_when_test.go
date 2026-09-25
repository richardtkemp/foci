package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pretoolWhenRules are #2029 candidates 1, 9 and 31, which need live state
// and so a when-check (#2034). The paths are the real ones; the test points
// them at temp repos and a temp skills dir.
const pretoolWhenRules = `
[[agents]]
id = "clutch"
backend = "claude-code"

# 1: a checkout/restore that would discard uncommitted edits to the file.
[[agents.backend_config.pretool_rules]]
name = "checkout_dirty_file"
tool = "Bash"
command = ['git (\S+ )*checkout (\S+ )*-- ', 'git (\S+ )*restore( |$)']
when = '''
shift                                  # $1 is "git"
repo=.
while [ $# -gt 0 ]; do                 # global options, up to the subcommand
  case $1 in
    -C) repo=$2; shift 2 ;;
    -c) shift 2 ;;
    checkout|restore) sub=$1; shift; break ;;
    *) shift ;;
  esac
done
files=() past= staged= worktree=
[ "$sub" = restore ] && past=1         # restore takes paths without --
for a; do
  case $a in
    --) past=1 ;;
    --staged|-S) staged=1 ;;
    --worktree|-W) worktree=1 ;;
    -*) ;;
    *) [ -n "$past" ] && files+=("$a") ;;
  esac
done
[ -n "$staged" ] && [ -z "$worktree" ] && exit 1   # only unstages
[ ${#files[@]} -gt 0 ] || exit 1
git -C "$repo" diff --quiet HEAD -- "${files[@]}"
case $? in 0) exit 1 ;; 1) exit 0 ;; *) exit 2 ;; esac
'''
reason = "That checkout/restore would discard uncommitted edits. To revert a file for a fail-arm, use git stash push -- <file> instead: the file goes back to HEAD the same way, and git stash pop brings the edits back."

# 9: a commit on main in a shared repo's main checkout, wherever it is aimed.
[[agents.backend_config.pretool_rules]]
name = "commit_on_main"
tool = "Bash"
command = 'git (\S+ )*commit( |$)'
when = '''
shift
repo=.
while [ $# -gt 0 ]; do
  case $1 in
    -C) repo=$2; shift 2 ;;
    -c) shift 2 ;;
    *) break ;;
  esac
done
top=$(git -C "$repo" rev-parse --show-toplevel 2>/dev/null) || exit 1
case $top in
  /home/rich/git/foci|/home/rich/git/foci-client) ;;
  *) exit 1 ;;
esac
[ "$(git -C "$repo" symbolic-ref --short -q HEAD)" = main ]
'''
reason = "Do not commit on main in the foci or foci-client main checkout. Work in a worktree on a feature branch and land it with make -C <worktree> land."

# 31: an edit to a deployed GOLDEN skill file, which a restart overwrites.
[[agents.backend_config.pretool_rules]]
name = "golden_skill_edit"
tool = "Edit"
input.file_path = '^/home/foci/shared/skills/'
when = 'head -n 5 -- "$TOOL_INPUT_FILE_PATH" 2>/dev/null | grep -q "GOLDEN:"'
reason = "This skill file is GOLDEN: it ships with foci, and the deployed copy under ~/shared/skills is overwritten on restart. Edit it in a worktree of /home/rich/git/foci, under shared/skills/."

[[agents.backend_config.pretool_rules]]
name = "golden_skill_write"
tool = "Write"
input.file_path = '^/home/foci/shared/skills/'
when = 'head -n 5 -- "$TOOL_INPUT_FILE_PATH" 2>/dev/null | grep -q "GOLDEN:"'
reason = "This skill file is GOLDEN: it ships with foci, and the deployed copy under ~/shared/skills is overwritten on restart. Edit it in a worktree of /home/rich/git/foci, under shared/skills/."
`

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeT(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPretoolTest_WhenRules runs candidates 1, 9 and 31 through
// ` + "`foci pretool test`" + `, so the when-checks run for real against a repo with
// a dirty file, a shared repo on main plus a worktree, and a skills dir with
// a GOLDEN file. A check that fails open would print "when failed open"
// and so fail the exact-output comparison.
func TestPretoolTest_WhenRules(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	repo := filepath.Join(root, "foci")
	wt := filepath.Join(root, "foci-wt-1")
	other := filepath.Join(root, "other")
	skills := filepath.Join(root, "skills")
	for _, d := range []string{repo, other, skills} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{repo, other} {
		gitT(t, d, "init", "-q", "-b", "main")
		writeT(t, filepath.Join(d, "a.txt"), "a\n")
		writeT(t, filepath.Join(d, "b.txt"), "b\n")
		gitT(t, d, "add", "a.txt", "b.txt")
		gitT(t, d, "commit", "-qm", "init")
	}
	gitT(t, repo, "worktree", "add", "-q", "-b", "feat", wt)
	writeT(t, filepath.Join(repo, "a.txt"), "edited\n")
	writeT(t, filepath.Join(skills, "golden.md"), "<!-- GOLDEN: ships with foci -->\n# x\n")
	writeT(t, filepath.Join(skills, "plain.md"), "# plain\n")

	body := strings.NewReplacer(
		"/home/rich/git/foci|/home/rich/git/foci-client", repo,
		"^/home/foci/shared/skills/", "^"+skills+"/",
	).Replace(pretoolWhenRules)
	path := writePretoolConfig(t, body)

	edit := func(p string) string { return `{"file_path":"` + p + `","old_string":"x","new_string":"y"}` }
	cases := []struct {
		args []string
		want string
	}{
		// 1: checkout_dirty_file
		{[]string{"--bash", "git checkout -- a.txt", "--cwd", repo}, "checkout_dirty_file"},
		{[]string{"--bash", "git checkout -- b.txt", "--cwd", repo}, ""},
		{[]string{"--bash", "git checkout -- .", "--cwd", repo}, "checkout_dirty_file"},
		{[]string{"--bash", "git -C " + repo + " checkout HEAD -- a.txt", "--cwd", other}, "checkout_dirty_file"},
		{[]string{"--bash", "git -C " + other + " checkout -- a.txt", "--cwd", repo}, ""},
		{[]string{"--bash", "git restore a.txt", "--cwd", repo}, "checkout_dirty_file"},
		{[]string{"--bash", "git restore --staged a.txt", "--cwd", repo}, ""},
		{[]string{"--bash", "echo x && git restore 'b.txt'", "--cwd", repo}, ""},
		{[]string{"--bash", "git checkout -b x", "--cwd", repo}, ""},
		{[]string{"--bash", `git commit -m "then git checkout -- a.txt"`, "--cwd", other}, ""},
		// 9: commit_on_main
		{[]string{"--bash", "git commit -m x", "--cwd", repo}, "commit_on_main"},
		{[]string{"--bash", "git add b.txt && git -c core.x=y -C " + repo + " commit -qm x", "--cwd", other}, "commit_on_main"},
		{[]string{"--bash", "git commit -m x", "--cwd", wt}, ""},
		{[]string{"--bash", "git -C " + wt + " commit -m x", "--cwd", repo}, ""},
		{[]string{"--bash", "git commit -m x", "--cwd", other}, ""},
		{[]string{"--bash", "git log -1", "--cwd", repo}, ""},
		// 31: golden_skill_edit / golden_skill_write
		{[]string{"--tool", "Edit", "--input", edit(filepath.Join(skills, "golden.md"))}, "golden_skill_edit"},
		{[]string{"--tool", "Edit", "--input", edit(filepath.Join(skills, "plain.md"))}, ""},
		{[]string{"--tool", "Write", "--input", `{"file_path":"` + filepath.Join(skills, "golden.md") + `","content":"x"}`}, "golden_skill_write"},
		{[]string{"--tool", "Write", "--input", `{"file_path":"` + filepath.Join(skills, "new.md") + `","content":"GOLDEN:"}`}, ""},
		{[]string{"--tool", "Edit", "--input", edit(filepath.Join(root, "golden.md"))}, ""},
	}
	for _, c := range cases {
		var out bytes.Buffer
		if err := cmdPretool(append([]string{"--config", path, "--agent", "clutch", "test"}, c.args...), &out); err != nil {
			t.Fatalf("%q: %v", c.args, err)
		}
		want := c.want
		if want == "" {
			want = "no match"
		}
		if got := strings.TrimSpace(out.String()); got != want {
			t.Errorf("%q: got %q, want %q", c.args, got, want)
		}
	}
}

// TestPretoolTest_WhenFailOpenShown: a check that errors is printed even
// without -v, since it silently lets calls through.
func TestPretoolTest_WhenFailOpenShown(t *testing.T) {
	path := writePretoolConfig(t, `
[[agents]]
id = "clutch"
backend = "claude-code"
[[agents.backend_config.pretool_rules]]
name = "broken"
tool = "Read"
when = "exit 7"
reason = "r"
`)
	var out bytes.Buffer
	if err := cmdPretool([]string{"--config", path, "--agent", "clutch", "test", "--tool", "Read"}, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.HasPrefix(got, "no match\n") || !strings.Contains(got, "when failed open: rule broken: when: exit status 7") {
		t.Errorf("output = %q", got)
	}
}
