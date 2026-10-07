package autoapprove

import (
	"testing"
)

// #2216: three shell expansions turn an auto-approved read-only command into
// code execution or a file write, and unlike #2213 the agent can type them —
// no planted file name is needed. These tables pin the rules that close them
// on every flag-checked command (an unsafeFlags entry, or sqlite3), and that
// commands without unsafe-flag machinery keep their behaviour.
//
//   - Quoting a flag does not change what the program receives: every flag
//     and argument check reads the words' shell-visible text.
//   - A brace group the shell would expand (`{"--pre=./x.sh",}`) can build a
//     flag out of quoted parts, so liveness is tracked across the quotes.
//   - A live $(…)/$((…))/backtick argument prompts instead of being
//     recursively approved: the OUTER command receives the substituted text.
//   - Variables in arguments are resolved from inline assignments and the
//     backend environment snapshot, then checked as if typed; anything that
//     cannot be resolved prompts.

// TestQuotedFlagsNotAutoApproved proves requirement 1: every flag and
// argument check compares shell-visible text, so quoting all or part of a
// dangerous flag (or a sed command, or a sqlite3 dot-command) cannot move it
// past the tables. Every row was approved before #2216, which compared the
// quoted token text.
func TestQuotedFlagsNotAutoApproved(t *testing.T) {
	readonly := parseAutoApproveRules(CommonReadonlyRules)
	tests := []struct {
		name  string
		rules []Rule // nil → the built-in readonly set
		cmd   string
	}{
		// Flag tables on shell-visible text.
		{"rg quoted long flag", nil, "rg '--pre=./x.sh' foo"},
		{"rg half-quoted long flag", nil, `rg "--pre"=./x.sh foo`},
		{"sed quoted short flag", nil, "sed '-i' s/a/b/ f"},
		{"sed flag split across quotes", nil, "sed -n'i' p f"},
		{"sort quoted short flag", nil, `sort "-o" out f`},
		{"printf quoted short flag", parseAutoApproveRules([]string{"Bash:printf"}), "printf '-v' x y"},
		{"git quoted subcommand", parseAutoApproveRules([]string{"Bash:git *"}), "git 'config' core.pager x"},
		// Argument checkers on shell-visible text.
		{"sed command split out of quotes", nil, "sed ''e f"},
		{"sed command half-quoted", nil, "sed 's/a/b/w'out f"},
		// Compound commands: one offending segment blocks the whole command.
		{"compound and", nil, "ls && rg '--pre=./x.sh' foo"},
		{"compound semicolon", nil, `echo x; sort "-o" out f`},
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

// TestQuotedCommandNameNotAutoApproved proves requirement 1's command-name
// half: the command name used to select flag-checking machinery is read from
// the shell-visible text too, so `"sed" -i …` or `se''d -i …` is flag-checked
// under a rule broad enough (tool-name-only Bash) to match the quoted form.
func TestQuotedCommandNameNotAutoApproved(t *testing.T) {
	rules := parseAutoApproveRules([]string{"Bash"})
	for _, cmd := range []string{
		`"sed" -i s/a/b/ f`,
		`"sort" -o out f`,
		`se''d -i s/a/b/ f`,
	} {
		assertApproval(t, rules, cmd, false)
	}
}

// TestSqliteChecksUseShellVisibleText proves requirement 1 for sqlite3's
// whole-vector check, in both directions: a dot-command assembled from quoted
// parts (`'.'shell`) stops approving, and a quoted `-readonly` flag is still
// the `-readonly` the check requires (before #2216 the quoted form was
// rejected by accident, because the check compared quoted token text).
func TestSqliteChecksUseShellVisibleText(t *testing.T) {
	readonly := parseAutoApproveRules(CommonReadonlyRules)
	assertApproval(t, readonly, "sqlite3 -readonly db '.'shell", false)

	blanket := parseAutoApproveRules([]string{"Bash"})
	assertApproval(t, blanket, `sqlite3 '-readonly' db.sql 'SELECT 1'`, true)
}

// TestBraceExpansionNotAutoApproved proves requirement 2: a brace group the
// shell expands — an unquoted `{`, a later unquoted `}`, and an unquoted `,`
// or `..` between them — is refused even when parts of it are quoted, because
// the AST's literal scan (litContainsBraceExpansion) only sees one Lit at a
// time and quoting splits the word into several parts. These are the forms
// that hid from it; the all-unquoted form is refused by the AST walk itself
// (see TestAllUnquotedBraceWordRefusedByAstWalk).
func TestBraceExpansionNotAutoApproved(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		`rg foo {"--pre=./x.sh",}`,
		`rg foo {'--pre=./x.sh',x}`,
		`sort {"-o",out} f`,
		`rg foo --pr{"e",}=./x.sh`,
		// Every segment of a compound command, and loop bodies.
		`ls && sort {"-o",out} f`,
		`for f in x; do sort {"-o",out} f; done`,
	} {
		assertApproval(t, rules, cmd, false)
	}
}

// TestContainsUnsafeFlagsBraceRule is the unit-level view of the brace rule:
// containsUnsafeFlags reports every brace group that expands (want true) —
// including the ticket's two verified inputs — and leaves braces the shell
// does not expand alone (want false): quoted braces, and groups with no
// unquoted `,` or `..` between them.
func TestContainsUnsafeFlagsBraceRule(t *testing.T) {
	tests := []struct {
		segment string
		want    bool
	}{
		// The ticket's two verified inputs.
		{"rg foo {--pre=./x.sh,}", true},
		{"sort {-o,out} f", true},
		// Quoted parts inside the braces do not make the word safe.
		{`rg foo {"--pre=./x.sh",}`, true},
		{`rg foo {'--pre=./x.sh',x}`, true},
		{`sort {"-o",out} f`, true},
		{`rg foo --pr{"e",}=./x.sh`, true},
		// Sequence form.
		{"sort {1..9} f", true},
		// Braces the shell does not expand → safe.
		{"rg 'a{1,2}' f", false},
		{"go list -f {{.Dir}} ./...", false},
		{"rg foo {x}", false},
		// Commands without unsafe-flag machinery keep today's behaviour.
		{"cat {a,b} f", false},
		{"ls {a,b}", false},
	}
	for _, tt := range tests {
		if got := containsUnsafeFlags(tt.segment); got != tt.want {
			t.Errorf("containsUnsafeFlags(%q) = %v, want %v", tt.segment, got, tt.want)
		}
	}
}

// TestBracesBashDoesNotExpandStayApproved is the characterisation half of
// requirement 3: a word whose braces are quoted, or with no unquoted `,`/`..`
// between them, is not brace-expanded by bash and keeps approval. `go` is
// flag-checked (wordFlags -vettool), so its `-f {{.Dir}}` template row proves
// the separator rule rather than the gate.
func TestBracesBashDoesNotExpandStayApproved(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		"rg 'a{1,2}' f",
		"go list -f {{.Dir}} ./...",
	} {
		assertApproval(t, rules, cmd, true)
	}
}

// TestAllUnquotedBraceWordRefusedByAstWalk pins the pre-existing AST literal
// walk (litContainsBraceExpansion): the all-unquoted brace word
// `rg foo {--pre=./x.sh,}` is one Lit, so the walk already refuses it without
// any #2216 rule — as does a DOUBLE-quoted brace group, whose content the
// parser keeps as a Lit inside the DblQuoted node the walk descends into
// (bash itself would not expand it; refusing it is the walk's deliberate
// over-approximation). The #2216 scanner rule covers the remaining
// quoted-part forms the walk cannot see; these rows pin that its cases stay
// refused.
func TestAllUnquotedBraceWordRefusedByAstWalk(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		"rg foo {--pre=./x.sh,}",
		`rg foo "{a,b}" f`,
	} {
		assertApproval(t, rules, cmd, false)
	}
}

// TestCommandSubstitutionArgsNotAutoApproved proves requirement 4's first
// half: a live $(…), $((…)) or backtick in an argument word of a flag-checked
// command blocks approval. The recursive walk approves a substitution whose
// inner commands are approved — `echo` and `base64` both are — but the outer
// command then receives the substituted TEXT as an argument, which is exactly
// the bypass: `rg foo $(echo LS1wcmU9Li94LnNo | base64 -d)` runs
// `rg foo --pre=./x.sh`.
func TestCommandSubstitutionArgsNotAutoApproved(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		"rg foo $(echo LS1wcmU9Li94LnNo | base64 -d)",
		"rg foo $(pwd)",
		`rg foo "$(echo x)"`,
		"rg foo `echo x`",
		"rg foo $((1+1))",
		"sed -n p $(echo f)",
		"sort $(echo f)",
		// Every segment of a compound command.
		"ls && rg foo $(echo x)",
	} {
		assertApproval(t, rules, cmd, false)
	}
}

// TestVariableArgsResolvedAndChecked proves requirement 4's resolution rule:
// variables in arguments of flag-checked commands are resolved (inline
// assignments first, then the environment snapshot) and the resulting text is
// checked as if it had been typed — an unquoted value keeps its glob
// liveness, and a value with whitespace word-splits, so a resolved flag
// cannot hide behind a variable.
func TestVariableArgsResolvedAndChecked(t *testing.T) {
	readonly := parseAutoApproveRules(CommonReadonlyRules)
	tests := []struct {
		name  string
		rules []Rule // nil → the built-in readonly set
		cmd   string
		env   map[string]string // nil → matchAutoApprove (process env)
		want  bool
	}{
		// Inline assignment carrying a dangerous flag value.
		{"inline pre flag", nil, "P=--pre=./x.sh; rg foo $P", nil, false},
		// Unquoted glob value: expansion is globbed by the shell.
		{"inline glob value", nil, "X='*'; rg foo $X", nil, false},
		// Whitespace in an unquoted value word-splits into new arguments.
		{"inline split flag", nil, "X='-i x'; sed $X s/a/b/ f", nil, false},
		// Environment snapshot: resolved, then checked the same way.
		{"env dangerous flag", nil, "rg foo $PAGER", map[string]string{"PAGER": "--pre=./x.sh"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules := tt.rules
			if rules == nil {
				rules = readonly
			}
			var got bool
			if tt.env != nil {
				got, _ = MatchWithEnv(rules, "Bash", bashInput(t, tt.cmd), tt.env)
			} else {
				got = matchAutoApprove(rules, "Bash", bashInput(t, tt.cmd))
			}
			if got != tt.want {
				t.Errorf("approval of %q = %v, want %v", tt.cmd, got, tt.want)
			}
		})
	}
}

// TestUnresolvableVariableArgsNotAutoApproved proves requirement 4's
// fail-closed rule: a variable that cannot be resolved — not assigned earlier
// in the line, not in the environment, or set by a construct the tracker does
// not model (a for-loop iterator) — blocks approval of a flag-checked
// command. `for f in *; do rg foo "$f"; done` is the #2213 glob moved into a
// loop variable: quoting the reference does not help, the VALUE is the glob.
func TestUnresolvableVariableArgsNotAutoApproved(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	assertApproval(t, rules, "for f in *; do rg foo \"$f\"; done", false)

	// An environment with nothing in it: $FOCI_UNSET_2216 has no value.
	got, _ := MatchWithEnv(rules, "Bash", bashInput(t, "rg foo $FOCI_UNSET_2216"), map[string]string{})
	if got {
		t.Error("an unresolvable variable argument must not be auto-approved")
	}
}

// TestAnsiCLocaleQuotingChecked proves requirement 4's quoting rule for
// $'…' (ANSI-C) and $"…" (locale): both produce argument text the plain
// scanner does not show, so they are resolved to their literal content and
// checked like quoted text — including hex-escaped content.
func TestAnsiCLocaleQuotingChecked(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		`rg foo $'--pre=./x.sh'`,
		`rg foo $'--\x70re=./x.sh'`,
		`rg foo $"--pre=./x.sh"`,
		`sed $'-i' s/a/b/ f`,
	} {
		assertApproval(t, rules, cmd, false)
	}
}

// TestVariableResolutionKeepsSafeCommandsApproved pins requirement 4's
// keep-approval half: resolution must not over-block. A resolved path with no
// flag, glob or brace shape approves exactly as if typed; a QUOTED reference
// keeps its value literal even when the value is a glob; and a value resolved
// from the backend environment snapshot is checked like typed text.
func TestVariableResolutionKeepsSafeCommandsApproved(t *testing.T) {
	// Inline symbol table: the git -C pattern is explicitly safe (P2-8).
	gitRules := parseAutoApproveRules([]string{"Bash:git *"})
	assertApproval(t, gitRules, "W=/srv/x; git -C $W log", true)

	// A quoted reference keeps the glob literal.
	readonly := parseAutoApproveRules(CommonReadonlyRules)
	assertApproval(t, readonly, "X='*'; rg foo \"$X\"", true)

	// Environment snapshot: $HOME resolves to a path; the unquoted glob that
	// follows cannot yield a flag because every expansion starts with '/'.
	got, _ := MatchWithEnv(readonly, "Bash", bashInput(t, "rg foo $HOME/*"), map[string]string{"HOME": "/srv/x"})
	if !got {
		t.Error("rg foo $HOME/* with HOME=/srv/x resolves to a rooted path and must stay approved")
	}
}

// TestAnsiCQuotedHarmlessArgsStayApproved pins that harmless ANSI-C content
// resolves to its literal text and keeps the approval it already had.
func TestAnsiCQuotedHarmlessArgsStayApproved(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	assertApproval(t, rules, `rg foo $'a\tb' f`, true)
}

// TestLiteralDollarStaysApproved pins requirement 4's literal-$ rule: a `$`
// that is not a variable or substitution reference — a sed last-line
// address, an escaped `\$` inside double quotes, a regex anchor — carries no
// expansion and keeps approval.
func TestLiteralDollarStaysApproved(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		`sed -n '$p' f`,
		`sed -n "1,\$p" f`,
		`rg ^foo$ f`,
	} {
		assertApproval(t, rules, cmd, true)
	}
}

// TestExpansionScopeForNonFlagCheckedCommands pins requirement 5: the #2216
// brace, substitution and variable rules attach only to flag-checked
// commands. Commands without an unsafeFlags entry keep their pre-#2216
// behaviour — variables pass through unresolved, substitutions keep the
// recursive inner-command validation, ANSI-C quoting is inert.
func TestExpansionScopeForNonFlagCheckedCommands(t *testing.T) {
	rules := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		"ls $HOME",
		`cat "$f"`,
		"for f in *.go; do ls $f; done",
		"echo $'x'",
		"cat $(echo /etc/hosts)",
		"grep pattern $(find . -name '*.go')",
	} {
		assertApproval(t, rules, cmd, true)
	}
}
