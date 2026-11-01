package autoapprove

import "testing"

// TestReadonlyGitApproves proves #2734's approve rows: the read-only rule set
// approves the read-only git subcommands in their common forms, after the
// allowlisted top-level options, and inside a pipeline.
func TestReadonlyGitApproves(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		"git status",
		"git status --porcelain -b",
		"git log",
		"git log --oneline -20",
		"git log -p -1 -- internal/x.go",
		"git log --format='%h %an %s' -5",
		"git log --grep=fix --since=2.weeks",
		"git show HEAD",
		"git show --stat HEAD~2",
		"git diff",
		"git diff --stat origin/main...HEAD",
		"git diff --no-index a b",
		"git blame -L 10,20 f.go",
		"git rev-parse --abbrev-ref HEAD",
		"git ls-files -m",
		"git ls-tree -r HEAD",
		"git cat-file -p HEAD",
		"git cat-file -t HEAD",
		"git cat-file -s HEAD:f.txt",
		"git describe --tags --always",
		"git shortlog -sn HEAD",
		"git reflog",
		"git reflog -10",
		"git reflog show main",
		"git reflog exists refs/heads/main",
		"git grep -n needle",
		"git branch",
		"git branch -a",
		"git branch -vv",
		"git branch --list 'wt-*'",
		"git branch -l 'wt-*'",
		"git branch --contains HEAD",
		"git branch --merged=main",
		"git branch --show-current",
		"git branch --format='%(refname:short)' --sort=-committerdate",
		"git tag",
		"git tag -l 'v1*'",
		"git tag --list",
		"git tag -n3 -l",
		"git tag --points-at HEAD",
		"git remote",
		"git remote -v",
		"git remote get-url origin",
		"git remote get-url --push origin",
		"git -C /home/rich/git/foci log -1",
		"git --no-pager -C /tmp/r diff",
		"git --git-dir=/tmp/r/.git log",
		"git --no-optional-locks status",
		"git log --oneline | head -5",
		"git log -1 2>/dev/null",
		"git \"log\" -1",
	} {
		assertApproval(t, rules, cmd, true)
	}
}

// TestReadonlyGitRefusesExecAndWrite proves #2734's refuse rows: every exec
// vector the command line can name, and every write form of the listed
// subcommands, asks under the read-only rule set. Each row is a form the
// rule's prefix-free matching would otherwise have to judge; git's own
// rejection of a spelling (an abbreviation the revision parser refuses) does
// not make approving it right.
func TestReadonlyGitRefusesExecAndWrite(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		// File output.
		"git log --output=/tmp/x",
		"git log --output /tmp/x",
		"git diff --output=/tmp/x",
		"git show --output=/tmp/x HEAD",
		"git log -- --output=/tmp/x",
		// Drivers named on the command line.
		"git log -p --ext-diff",
		"git diff --ext-diff",
		"git show --textconv HEAD:f",
		"git diff --textconv",
		"git blame --textconv f",
		"git cat-file --textconv HEAD:f",
		"git cat-file --filters HEAD:f",
		"git cat-file --filt HEAD:f",
		"git cat-file --batch --filters",
		"git grep --textconv x",
		"git grep -O x",
		"git grep --open-files-in-pager=./x x",
		// Signature verification runs gpg.program.
		"git log --show-signature",
		"git show --show-signature HEAD",
		"git log --format=%G?",
		"git log --pretty=format:%GS",
		"git log --format='%h %GK'",
		"git branch --format='%(signature)'",
		"git tag --sort=signature",
		// Help runs the configured man viewer.
		"git log --help",
		"git status --help",
		"git --help log",
		"git branch --hel",
		// Top-level options outside the allowlist.
		"git -c core.pager=./x log",
		"git -c core.fsmonitor=./x status",
		"git --config-env=core.pager=X log",
		"git --exec-path=/tmp log",
		"git -p log",
		"git --paginate log",
		"git --attr-source=HEAD diff",
		"git -- log",
		"git -C",
		"git",
		// Subcommands outside the set.
		"git commit -m x",
		"git add f",
		"git checkout main",
		"git fetch",
		"git config --list",
		"git log config",
		"git help log",
		"git rev-list HEAD",
		"git format-patch -1",
		// branch: only list forms.
		"git branch newname",
		"git branch -v newname",
		"git branch -d old",
		"git branch -D old",
		"git branch -m a b",
		"git branch -c a b",
		"git branch --copy a b",
		"git branch -f main HEAD",
		"git branch -u origin/main",
		"git branch --set-upstream-to=origin/main",
		"git branch --unset-upstream",
		"git branch --edit-description",
		"git branch --track x origin/x",
		"git branch --cont HEAD",
		"git branch -- newname",
		// tag: only list forms.
		"git tag v2",
		"git tag -a v2 -m x",
		"git tag -d v1",
		"git tag -v v1",
		"git tag -s v2",
		"git tag -f v1",
		"git tag -n x",
		// reflog: show and exists only.
		"git reflog expire --all",
		"git reflog delete HEAD@{1}",
		"git reflog -1 --output=/tmp/x",
		// remote: -v and get-url only.
		"git remote show origin",
		"git remote add x ./y",
		"git remote set-url origin ./y",
		"git remote remove origin",
		"git remote prune origin",
		"git remote -v update",
		"git remote get-url",
		// Words the shell decides.
		"git log $X",
		"git log \"$X\"",
		"git $SUB",
		"git log $(echo --output=/tmp/x)",
		"git reflog e*",
		"git log {--output=/tmp/x,-1}",
	} {
		assertApproval(t, rules, cmd, false)
	}
}

// TestReadonlyGitRuleIsScoped proves the read-only rule judges only its own
// approval: a broader rule keeps approving the forms the read-only rule
// refuses (Dick's `git branch *` creates branches by design), and the
// reserved pattern does not prefix-match a real command.
func TestReadonlyGitRuleIsScoped(t *testing.T) {
	both := parseAutoApproveRules(append([]string{"Bash:git branch *"}, CommonReadonlyRules...))
	assertApproval(t, both, "git branch newname", true)

	only := parseAutoApproveRules([]string{ReadonlyGitRule})
	assertApproval(t, only, "git branch newname", false)
	assertApproval(t, only, "git (read-only)", false)
	assertApproval(t, only, "git log", true)

	// The same pattern string written by hand is the same rule.
	if r := parseAutoApproveRule(ReadonlyGitRule); !r.readonlyGit {
		t.Fatal("ReadonlyGitRule did not compile to the read-only git matcher")
	}
	if r := parseAutoApproveRule("Bash:git log"); r.readonlyGit {
		t.Fatal("a plain git prefix rule compiled to the read-only git matcher")
	}
}

// TestReadonlyGitDisplay proves the prompts can name the rule: DisplayRule
// spells out every subcommand, and passes every other rule through with the
// Bash: prefix dropped.
func TestReadonlyGitDisplay(t *testing.T) {
	d := DisplayRule(ReadonlyGitRule)
	for _, sub := range ReadonlyGitSubcommands() {
		if !containsWord(d, sub) {
			t.Errorf("DisplayRule(ReadonlyGitRule) = %q lacks %q", d, sub)
		}
	}
	if got := DisplayRule("Bash:crontab -l"); got != "crontab -l" {
		t.Errorf("DisplayRule(Bash:crontab -l) = %q", got)
	}
	if got := DisplayRule("Read"); got != "Read" {
		t.Errorf("DisplayRule(Read) = %q", got)
	}
}

func containsWord(s, w string) bool {
	for i := 0; i+len(w) <= len(s); i++ {
		if s[i:i+len(w)] != w {
			continue
		}
		before := i == 0 || !isWordByte(s[i-1])
		after := i+len(w) == len(s) || !isWordByte(s[i+len(w)])
		if before && after {
			return true
		}
	}
	return false
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c == '-'
}

// TestReadonlyGitResolvedVariableAsks proves the rule reads only literal
// words: a variable whose value is a write flag asks, whether the value comes
// from the backend's environment or an inline assignment. The global flag
// checks judge the resolved value only against every git rule's refusals
// (-c, config, …), so without the literal-word check the rule would read
// `$X` as a revision and approve.
func TestReadonlyGitResolvedVariableAsks(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	env := map[string]string{"X": "--output=/tmp/x", "B": "newname"}
	assertApprovalEnv(t, rules, env, "git log $X", false)
	assertApprovalEnv(t, rules, env, "git log \"$X\"", false)
	assertApprovalEnv(t, rules, env, "git branch $B", false)
	assertApprovalEnv(t, rules, nil, "X=--output=/tmp/x; git log $X", false)
	// Control: the same commands typed literally are judged the same way,
	// and a literal safe form still approves with that environment.
	assertApprovalEnv(t, rules, env, "git log --output=/tmp/x", false)
	assertApprovalEnv(t, rules, env, "git log -1", true)
}
