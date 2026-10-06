package autoapprove

import (
	"testing"
)

// #2213: the shell expands an unquoted glob BEFORE the command runs, so a
// file name planted in the working directory (a cloned untrusted repo) can
// become a flag — `rg foo *` in a directory containing a file named
// --pre=./x.sh runs `rg foo --pre=./x.sh …`, which executes x.sh. These
// tables pin the rule that arguments whose expansion could begin with '-'
// are prompted, not auto-approved, on every command that has unsafe-flag
// machinery — and that globs which cannot produce a flag keep approval.

// assertApproval checks the auto-approve verdict of one Bash command against
// rules, so every #2213 table row reads "command → verdict" without
// repeating the match plumbing.
func assertApproval(t *testing.T, rules []Rule, cmd string, want bool) {
	t.Helper()
	got := matchAutoApprove(rules, "Bash", bashInput(t, cmd))
	if got != want {
		t.Errorf("matchAutoApprove(rules, Bash, %q) = %v, want %v", cmd, got, want)
	}
}

// TestFlagCapableGlobArgsNotAutoApproved proves requirement 1: for every
// command with unsafe-flag machinery (an unsafeFlags entry, or sqlite3's own
// whole-vector check), an argument whose glob expansion could begin with '-'
// blocks auto-approval. Two shapes qualify: a word whose FIRST character is
// a live * ? [ (a matched file name becomes the whole argument), and a word
// starting with '-' that contains a live glob (matched names extend the
// flag-shaped prefix). Every row is auto-approved by the pre-#2213 code,
// which only inspected literal flag tokens.
func TestFlagCapableGlobArgsNotAutoApproved(t *testing.T) {
	readonly := parseAutoApproveRules(CommonReadonlyRules)
	tests := []struct {
		name  string
		rules []Rule // nil → the built-in readonly set
		cmd   string
	}{
		// The ticket's pair, first half: bare glob metacharacters.
		{"rg bare star", nil, "rg foo *"},
		{"rg bare question", nil, "rg foo ?"},
		{"rg bare bracket", nil, "rg foo [*]"},
		{"rg leading glob then literal", nil, "rg foo *x"},
		// An empty quoted prefix contributes nothing: the live glob is
		// still the word's first shell-visible character.
		{"rg empty quoted prefix", nil, "rg foo ''*"},
		// A '-'-leading word with a live glob extends into a flag value.
		{"rg dash glob", nil, "rg foo --*"},
		{"rg quoted dash unquoted star", nil, "rg foo '-'*"},
		{"rg escaped dash unquoted star", nil, `rg foo \-*`},
		// The same shape across the other flag-checked commands.
		{"find", nil, "find *"},
		{"sed", nil, "sed -n p *"},
		{"sort", nil, "sort *"},
		{"go vet", nil, "go vet *"},
		{"yq", nil, "yq '.' *"},
		{"printf", parseAutoApproveRules([]string{"Bash:printf"}), "printf 'x' *"},
		{"git status", parseAutoApproveRules([]string{"Bash:git *"}), "git status *"},
		{"git grep", parseAutoApproveRules([]string{"Bash:git *"}), "git grep pattern *"},
		// sqlite3 has no unsafeFlags entry — its own check gates it in.
		{"sqlite3 readonly", nil, "sqlite3 -readonly db.sql *"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules := tt.rules
			if rules == nil {
				rules = readonly
			}
			assertApproval(t, rules, tt.cmd, false)
		})
	}
}

// TestGlobRuleHasNoDoubleDashExemption proves requirement 3: an unquoted
// glob after -- is still unsafe. Some commands (notably find) do not honour
// -- for every later argument, so the rule stays uniform.
func TestGlobRuleHasNoDoubleDashExemption(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		"rg -- *",
		"rg foo -- *",
		"sort -- *",
	} {
		assertApproval(t, rules, cmd, false)
	}
}

// TestCompoundCommandGlobRuleAppliesToEverySegment proves requirement 4: the
// glob rule reaches every segment of a compound command (&&, ||, ;, |), like
// the existing unsafe-flag check — one flag-capable glob anywhere blocks the
// whole command.
func TestCompoundCommandGlobRuleAppliesToEverySegment(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		"ls && rg foo *",
		"echo hi; sed -n p *",
		"cat /etc/hosts | sort *",
		"ls /nonexistent || rg foo ?",
	} {
		assertApproval(t, rules, cmd, false)
	}
}

// TestGitGrepPagerFlagsNotAutoApproved proves requirement 5: git grep's
// pager flags (-O, -O<pager>, --open-files-in-pager[=<cmd>]) run a pager or
// arbitrary command over the matched files — the same exec class as
// rg --pre — and must not be auto-approved. Detection is quote-aware
// (quoting does not change what git receives) and positional (git global
// options may precede the subcommand), so quoted, mixed-quoted, escaped and
// value-taking-global forms are all caught.
func TestGitGrepPagerFlagsNotAutoApproved(t *testing.T) {
	rules := parseAutoApproveRules([]string{"Bash:git *", "Bash:echo"})
	for _, cmd := range []string{
		"git grep -O pattern",
		"git grep -Oless pattern",
		"git grep --open-files-in-pager pattern",
		"git grep --open-files-in-pager=less pattern",
		// Global options before the subcommand.
		"git -C /tmp grep -O pattern",
		"git --no-pager grep -O pattern",
		"git --attr-source HEAD grep -O./evil.sh pattern",
		"git --attr-source=HEAD grep -O pattern",
		"git --work-tree /tmp grep -Oless pattern",
		// Quoting does not change what git receives.
		`git "grep" -O pattern`,
		"git grep '-Oevil.sh' pattern",
		"git grep '-O' pattern",
		"git grep '--open-files-in-pager=evil.sh' pattern",
		`git grep \-Oevil.sh pattern`,
		"git grep '-O'evil.sh pattern",
		// Compound: the pager flag in one segment blocks the whole command.
		"git grep -O pattern && echo done",
	} {
		assertApproval(t, rules, cmd, false)
	}
}

// TestContainsUnsafeFlagsGlobRule is the unit-level view of the same rule:
// containsUnsafeFlags itself reports the glob-carrying segments (want true)
// and leaves every glob that cannot yield a flag, every command without
// unsafe-flag machinery, and every plain git grep alone (want false).
func TestContainsUnsafeFlagsGlobRule(t *testing.T) {
	tests := []struct {
		segment string
		want    bool
	}{
		// Flag-capable globs on flag-checked commands → unsafe.
		{"rg foo *", true},
		{"rg foo ''*", true},
		{"rg foo --*", true},
		{"rg foo '-'*", true},
		{`rg foo \-*`, true},
		{"find *", true},
		{"sed -n p *", true},
		{"sort *", true},
		{"go vet *", true},
		{"yq '.' *", true},
		{"printf 'x' *", true},
		{"git status *", true},
		{"git grep pattern *", true},
		{"sqlite3 -readonly db.sql *", true},
		// No -- exemption.
		{"rg -- *", true},
		{"rg foo -- *", true},
		{"sort -- *", true},
		// git grep pager flags, in every quoting form.
		{"git grep -O pattern", true},
		{"git --attr-source HEAD grep -O pattern", true},
		{"git grep '-Oevil.sh' pattern", true},
		{"git grep '--open-files-in-pager=evil.sh' pattern", true},
		// Globs that cannot yield a flag → safe.
		{"rg foo '*.go'", false},
		{`rg foo "*.go"`, false},
		{`rg foo \*`, false},
		{"rg foo ./*", false},
		{"rg foo src/*.go", false},
		{"rg foo internal/**/x_test.go", false},
		{"rg foo x*", false},
		{"rg foo 'arg'*", false},
		{"rg foo '*'x", false},
		{"sort src/*.txt", false},
		{"go vet ./...", false},
		// Commands without unsafe-flag machinery ignore globs entirely.
		{"ls *", false},
		{"cat *.go", false},
		{"wc -l *", false},
		{"grep -r foo *", false},
		{"jq .name *", false},
		// git grep without a pager flag stays safe.
		{"git grep pattern", false},
		{"git grep -o pattern", false},
		{"git diff -Oorders.txt a b", false},
		{"git log --grep=foo", false},
	}
	for _, tt := range tests {
		if got := containsUnsafeFlags(tt.segment); got != tt.want {
			t.Errorf("containsUnsafeFlags(%q) = %v, want %v", tt.segment, got, tt.want)
		}
	}
}

// TestGlobsThatCannotYieldFlagsStayApproved is the characterisation half of
// requirement 2 (and the ticket's pair, second half): a glob word whose
// expansion cannot begin with '-' keeps approval. Quoted ('*.go', "*") and
// backslash-escaped (\*) metacharacters are literal to the shell, and a
// literal prefix before the glob (src/*.go, ./*, x*, 'arg'*) anchors every
// expansion to that prefix. '*'x is safe because its * is QUOTED: the only
// name it can match is the literal *x.
func TestGlobsThatCannotYieldFlagsStayApproved(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		"rg foo '*.go'",
		`rg foo "*.go"`,
		`rg foo \*`,
		"rg foo ./*",
		"rg foo src/*.go",
		"rg foo internal/**/x_test.go",
		"rg foo x*",
		"rg foo 'arg'*",
		"rg foo '*'x",
		"sort src/*.txt",
		"go vet ./...",
	} {
		assertApproval(t, rules, cmd, true)
	}
}

// TestGitGrepWithoutPagerFlagsStaysApproved characterises requirement 5's
// scope: only git's grep subcommand is pager-checked. Plain git grep (with
// any safe flags), value-taking globals before it, a quoted subcommand word,
// and other subcommands carrying -O or --grep (git diff -O<orderfile>,
// git log --grep=<pat>) all keep their pre-#2213 approval.
func TestGitGrepWithoutPagerFlagsStaysApproved(t *testing.T) {
	rules := parseAutoApproveRules([]string{"Bash:git *"})
	for _, cmd := range []string{
		"git grep pattern",
		"git grep -i pattern",
		"git grep -o pattern", // lowercase -o is --only-matching, not a pager
		"git diff -Oorders.txt a b",
		"git log --grep=foo",
		"git -C /tmp grep pattern",
		"git --attr-source HEAD grep pattern",
		`git "grep" pattern`,
	} {
		assertApproval(t, rules, cmd, true)
	}
}

// TestCommandsWithoutUnsafeFlagEntriesIgnoreGlobs characterises requirement
// 6: commands with no unsafe-flag entry (ls, cat, wc, grep, jq) ignore glob
// arguments entirely — their approval behaviour is unchanged by #2213.
func TestCommandsWithoutUnsafeFlagEntriesIgnoreGlobs(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		"ls *",
		"cat *.go",
		"wc -l *",
		"grep -r foo *",
		"jq .name *",
	} {
		assertApproval(t, rules, cmd, true)
	}
}
