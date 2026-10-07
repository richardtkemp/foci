package autoapprove

import (
	"fmt"
	"testing"

	"foci/internal/execguard"
)

// #2228: two gaps let a hidden command, flag or variable past the #2216
// quoting/brace/variable rules, because every name-based check read the
// PRINTED command word. These tables pin the fixes:
//
//   - A command word that is not plain literal text (or carries a live glob
//     or live brace) is resolved through the same resolver the argument path
//     uses, and the segment is judged on the resolved name exactly as if it
//     had been typed — flag checks, interceptor/var-setting/wrapper refusal,
//     the substitutability veto. A word that cannot resolve to exactly one
//     safe name refuses. Rules keep matching the printed text, so resolution
//     can only take an approval away.
//   - A live brace in a command word, in a shell interceptor's -c script
//     word, or in a declaration word (export/declare/local/readonly/typeset)
//     refuses: bash brace-expands those into a hidden command, script or
//     variable value.
//
// The keep-approved rows, the already-refused brace forms and the two pinned
// #2216 hunks are characterisation tests, listed in
// .factory/red-gate-exempt.txt; every other row was approved before #2228.

// TestNonLiteralCommandWordResolvedOrRefused proves requirement 1: under a
// blanket Bash rule — where the flag checks are the only guard left — every
// command word that is not plain literal text is either resolved and judged
// as the name bash runs (`$C`, `"$C"`, `${C}`, `se$S`, `$HOME/bin/x`) or
// refused outright (substitutions, arithmetic, unset/array/complex
// references, values that word-split or empty out, live globs and braces),
// in every context the walker validates: `&&`/pipe segments, subshells,
// command-substitution bodies, -c scripts and loop bodies.
func TestNonLiteralCommandWordResolvedOrRefused(t *testing.T) {
	blanket := parseAutoApproveRules([]string{"Bash"})
	for _, cmd := range []string{
		// Resolution reaches a name or argument the printed text hid.
		`C=rg; P=--pre=./x.sh; $C foo $P`,
		`C=sed; "$C" -i s/a/b/ f`,
		`C=sed; ${C} -i s/a/b/ f`,
		`S=d; se$S -i s/a/b/ f`,
		// The resolved word must be exactly one non-empty word.
		`E=; $E sed -i s/a/b/ f`,
		`C='sed -i'; $C s/a/b/ f`,
		// Unresolvable words fail closed: arrays, complex expansions.
		`a=(sed -i s/a/b/ f); "${a[@]}"`,
		`$((1)) x`,
		// Substitutions in the command word never resolve.
		`$(echo sed) -i s/a/b/ f`,
		"`echo sed` -i s/a/b/ f",
		// A live glob in the command word names no single file bash runs.
		`/usr/bin/se? -i s/a/b/ f`,
		`/usr/bin/s[e]d -i s/a/b/ f`,
		// The resolved name is judged as if typed: var-setting commands,
		// shell interceptors and wrappers-with-arguments all refuse.
		`C=eval; $C "sed -i s/a/b/ f"`,
		`C=bash; $C -c "sed -i s/a/b/ f"`,
		`C=env; $C sed -i s/a/b/ f`,
		// A resolved flag-checked name gets the #2216 argument rules.
		`C=rg; $C foo {"--pre=./x.sh",}`,
		// Every context the walker validates.
		`C=sed; ls && $C -i s/a/b/ f`,
		`C=sed; echo x | $C -i s/a/b/ f`,
		`(C=sed; $C -i s/a/b/ f)`,
		`echo $(C=sed; $C -i s/a/b/ f)`,
		`bash -c 'C=sed; $C -i s/a/b/ f'`,
		`C=sed; for f in x; do $C -i s/a/b/ f; done`,
	} {
		assertApproval(t, blanket, cmd, false)
	}
	// An unset variable names no command bash could be asked about. Nil env,
	// so the row cannot depend on the host process.
	if ok, _ := MatchWithEnv(blanket, "Bash", bashInput(t, "$NOPE -la"), nil); ok {
		t.Error("an unset variable as command word must not be auto-approved")
	}
}

// TestNonLiteralCommandWordVetoed proves requirement 1's veto half: the
// match-time substitutability check judges the RESOLVED name. With a writable
// ~/.local/bin/sqlite3 (TestMatchTimeVeto_WritableExecutableIsNotApproved's
// setup) and the blanket Bash rule — the printed segment `$C -readonly …`
// matches no sqlite3 rule, so a rule-shaped test would be vacuous — the
// command must be denied with a non-empty reason, proving the refusal is the
// veto explaining itself rather than an accidental no-match.
func TestNonLiteralCommandWordVetoed(t *testing.T) {
	withGuardEnv(t, execguard.Env{
		CanWrite:     func(p string) bool { return p == "/home/foci/.local/bin/sqlite3" },
		PathDirs:     []string{"/home/foci/.local/bin", "/usr/bin"},
		IsExecutable: func(p string) bool { return p == "/home/foci/.local/bin/sqlite3" },
		HomeDir:      "/home/foci",
	})
	rules := Compile([]string{"Bash"})
	ok, reason := MatchWithEnv(rules, "Bash", bashInput(t, `C=sqlite3; $C -readonly /tmp/x.db 'SELECT 1'`), nil)
	if ok {
		t.Error("a resolved command word naming a substitutable executable must not be auto-approved")
	}
	if reason == "" {
		t.Error("the veto on a resolved name must record its reason like the printed-path veto does")
	}
}

// TestCommandWordLiveBraceNotAutoApproved proves requirement 2's command-word
// half: a live brace in the command word — quoted parts or not — expands into
// command words bash runs (`{"sed",-i}` runs `sed -i`), which no printed-name
// check saw. The all-unquoted form `{sed,-i}` is already refused by the AST
// literal scan and lives with the other already-refused rows.
func TestCommandWordLiveBraceNotAutoApproved(t *testing.T) {
	blanket := parseAutoApproveRules([]string{"Bash"})
	for _, cmd := range []string{
		`{"sed",-i} s/a/b/ f`,
		`{"rg",--pre=./x.sh} foo`,
	} {
		assertApproval(t, blanket, cmd, false)
	}
}

// TestInterceptorScriptWordLiveBraceNotAutoApproved proves requirement 2's
// -c-script half: a live brace in the word after `bash -c` expands into
// scripts before any of them runs, while resolveStaticWord would validate the
// unexpanded text. Both the quoted-parts form and the escaped-spaces form hid
// from every check before #2228. (A brace inside a QUOTED script, as in
// `bash -c 'echo {a,b}'`, is not live in the word — that row is a
// characterisation test.)
func TestInterceptorScriptWordLiveBraceNotAutoApproved(t *testing.T) {
	blanket := parseAutoApproveRules([]string{"Bash"})
	for _, cmd := range []string{
		`bash -c {"sed -i s/a/b/ f",}`,
		`bash -c {sed\ -i\ s/a/b/\ f,}`,
	} {
		assertApproval(t, blanket, cmd, false)
	}
}

// TestDeclarationLiveBraceNotAutoApproved proves requirement 2's declaration
// half: bash brace-expands the arguments of export/declare/readonly/typeset
// (but not plain or command-prefix assignments), so `P={"",--pre=./x.sh}`
// sets P to `--pre=./x.sh` and a later `rg foo "$P"` would receive the flag
// the tables never saw. All four declaration commands refuse.
func TestDeclarationLiveBraceNotAutoApproved(t *testing.T) {
	readonly := parseAutoApproveRules(CommonReadonlyRules)
	for _, kw := range []string{"export", "declare", "readonly", "typeset"} {
		assertApproval(t, readonly, fmt.Sprintf(`%s P={"",--pre=./x.sh}; rg foo "$P"`, kw), false)
	}
}

// TestNonLiteralCommandWordKeepsApproval is a characterisation test: the
// keep-approved half of requirement 1. A command word that resolves to
// exactly one safe name keeps approval (`C=ls; $C -la`, `$HOME/bin/x -la`
// against the environment snapshot), and a literal lone `[` — the test
// command, with no closing `]` in its word — is not a live glob and stays
// approved.
func TestNonLiteralCommandWordKeepsApproval(t *testing.T) {
	blanket := parseAutoApproveRules([]string{"Bash"})
	assertApproval(t, blanket, `C=ls; $C -la`, true)
	assertApproval(t, blanket, `[ -f x ]`, true)
	got, _ := MatchWithEnv(blanket, "Bash", bashInput(t, `$HOME/bin/x -la`), map[string]string{"HOME": "/home/u"})
	if !got {
		t.Error(`$HOME/bin/x -la with HOME=/home/u resolves to a rooted non-substitutable name and must stay approved`)
	}
}

// TestCommandWordBraceFormsAlreadyRefused is a characterisation test: two
// brace forms were already refused before #2228 and must stay refused — the
// all-unquoted command word `{sed,-i}` is a single Lit the AST scan sees, and
// `bash -c 'echo {a,b}'` has its brace inside the quoted script, where it is
// not live in the word but the inner script's own Lit scan refuses it.
func TestCommandWordBraceFormsAlreadyRefused(t *testing.T) {
	blanket := parseAutoApproveRules([]string{"Bash"})
	assertApproval(t, blanket, `{sed,-i} s/a/b/ f`, false)
	assertApproval(t, blanket, `bash -c 'echo {a,b}'`, false)
}

// TestBraceScopeOutRowsStayApproved is a characterisation test: requirement
// 2's scope-out decisions. An ordinary command's brace arguments
// (`cat {"a",b}`) expand into argument text the agent could have typed, with
// the preamble and postscript kept in every alternative, so nothing new is
// reachable; and bash never brace-expands plain or command-prefix assignment
// words, so those keep today's verdict too.
func TestBraceScopeOutRowsStayApproved(t *testing.T) {
	readonly := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		`cat {"a",b}`,
		`cat {"a",b}.txt`,
		`export P=/srv; rg foo "$P"`,
		`P={"",x}; rg foo`,
		`P={"",x} rg foo`,
	} {
		assertApproval(t, readonly, cmd, true)
	}
}

// TestMutationGateSurvivorsPinned is a characterisation test (requirement 3):
// it pins the two #2216 hunks no test covered. Removing the ANSI-C decode
// branch of resolveWordPart looks stricter but lets the raw, undecoded
// escapes hide the flag (`X=$'\x2d-pre=./x.sh'` and
// `bash -c $'sed -\x69 …'` would approve); restoring stripOuterQuotes in
// sqliteCommandUnsafe would refuse `sqlite3 -readonly db "'.tables'"`, whose
// SQL string literal is not a dot-command. All four rows pass on main and
// must keep doing so.
func TestMutationGateSurvivorsPinned(t *testing.T) {
	readonly := parseAutoApproveRules(CommonReadonlyRules)
	assertApproval(t, readonly, `X=$'\x2d-pre=./x.sh'; rg foo $X`, false)
	assertApproval(t, readonly, `X=$'harmless'; rg foo $X`, true)
	assertApproval(t, readonly, `sqlite3 -readonly db "'.tables'"`, true)
	blanket := parseAutoApproveRules([]string{"Bash"})
	assertApproval(t, blanket, `bash -c $'sed -\x69 s/a/b/ f'`, false)
}
