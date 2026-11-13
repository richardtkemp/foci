package autoapprove

import (
	"strings"
	"testing"
)

// #3064 F2. Bash evaluates a variable's VALUE as an arithmetic expression,
// and an array subscript in that value can hold a command substitution, which
// bash runs: `x='a[$(id)]'; (( x ))` runs id. The read-only rule set used to
// approve it (the #2968 review's TestReviewArithmeticHiddenSubstitutionApproved,
// rewritten to assert the gate DENIES). Every row below was measured with
// bash 5.2 to run its hidden substitution.
func TestArithmeticHiddenSubstitutionDenied(t *testing.T) {
	ro := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		// The review's probe.
		`x='a[$(id)]'; (( x ))`,
		`(( x ))`, // x from the environment: its value is unknown text
		`x='a[$(id)]'; echo $((x))`,
		`x='a[$(id)]'; echo $(( $x ))`,
		`x='a[$(id)]'; echo $[x]`,
		`x='a[$(id)]'; let x`,
		`let x`,
		`x='a[$(id)]'; for ((i=0;i<x;i++)); do :; done`,
		// Subscripts.
		`x='a[$(id)]'; a=(1 2); echo ${a[x]}`,
		`x='a[$(id)]'; a[x]=1`,
		`x='a[$(id)]'; a=([x]=1)`,
		`x='a[$(id)]'; echo ${#a[x]}`,
		`x='a[$(id)]'; s=hello; echo ${s:x}`,
		`x='a[$(id)]'; s=hello; echo ${s:0:x}`,
		// Numeric comparisons in [[ ]] evaluate both operands.
		`x='a[$(id)]'; [[ $x -eq 1 ]]`,
		`[[ x -eq 1 ]]`,
		`[[ 1 -lt "$x" ]]`,
		`[[ "a[\$(id)]" -ge 1 ]]`,
		// Integer variables evaluate every assignment.
		`declare -i n; n='a[$(id)]'`,
		`local -ri n='a[$(id)]'`,
		// Name lookups evaluate the subscript in the name.
		`[[ -v 'a[$(id)]' ]]`,
		`[[ -R 'a[$(id)]' ]]`,
		`x='a[$(id)]'; [[ -v $x ]]`,
		`test -v 'a[$(id)]'`,
		`[ -v 'a[$(id)]' ]`,
		`x='a[$(id)]'; [ -v "$x" ]`,
		`f=-v; [ "$f" 'a[$(id)]' ]`,
		`[ "$(cat f1)" "$(cat f2)" ]`,
		`command test -v 'a[$(id)]'`,
		// Indirect expansion: x's value names the variable read.
		`x='a[$(id)]'; echo ${!x}`,
		`echo "${!1}"`,
		`a=(x); echo ${!a[0]}`,
		// Inside a -c script and a substitution, the same.
		`bash -c 'x=1; (( x ))'`,
		`echo "$(echo $((x)))"`,
	} {
		assertApproval(t, ro, cmd, false)
	}
}

// The ruling keeps pure-literal arithmetic approved, and the denial is scoped
// to arithmetic and subscripts: ordinary expansions and tests are untouched.
// These rows are the controls that make the denials above mean something.
func TestArithmeticLiteralControlsStayApproved(t *testing.T) {
	ro := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		`(( 1 + 2 ))`,
		`(( (2*3) % 4 ? 1 : 0 ))`,
		`echo $((1+2))`,
		`echo $(( 16#ff + 0x10 - 010 ))`,
		`echo $[1+2]`,
		`let 1+2`,
		`for ((;;)); do echo x; done`,
		`echo $(( $# + 1 ))`,
		`[[ 1 -eq 1 ]]`,
		`[[ -1 -lt +2 ]]`,
		`[[ $# -gt 0 ]]`,
		`[[ -v HOME ]]`,
		`[[ "$x" == a* ]]`,
		`[[ -n "$x" && -f /etc/hosts ]]`,
		`test -v HOME`,
		`[ -f /etc/hosts ]`,
		`x=/etc/hosts; [ -f "$x" ]`,
		`test -n "$(cat /etc/hostname)"`,
		`a=(1 2); echo "${a[@]}" "${a[*]}" ${a[0]} ${#a[@]}`,
		`a[0]=1; a=([1]=2)`,
		`echo "${!a[@]}" ${!HO*}`,
		`echo ${HOME:0:3} ${HOME: -2}`,
		`echo ${HOME} ${#HOME} ${HOME:-/tmp} ${HOME%/*}`,
		`declare -r n=1`,
	} {
		assertApproval(t, ro, cmd, true)
	}
}

// #3064 F3. The sqlite3 CLI's writefile() writes any file even with
// -readonly; -safe refuses it. The read-only rules used to approve
// `sqlite3 -readonly db "select writefile(...)"` (the #2968 review's
// TestReviewSQLiteWriteFunctionApproved, rewritten to assert the gate
// DENIES). The ruled fix requires BOTH flags.
func TestSQLiteWriteFunctionDenied(t *testing.T) {
	ro := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		`sqlite3 -readonly db "select writefile('/tmp/x','y')"`,
		`/usr/bin/sqlite3 -readonly db "select writefile('/tmp/x','y')"`,
		`sqlite3 -readonly db 'select 1'`,     // no -safe: prompts
		`sqlite3 -safe db 'select 1'`,         // no -readonly: prompts
		`sqlite3 -readonly -safe db`,          // SQL from stdin
		`sqlite3 -readonly -safe -cmd x db q`, // an extra option
		`sqlite3 -readonly -safe db '.shell id'`,
		`sqlite3 db -readonly -safe 'select 1'`, // flags must lead
	} {
		assertApproval(t, ro, cmd, false)
	}
}

func TestSQLiteReadonlySafeStaysApproved(t *testing.T) {
	ro := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		`sqlite3 -readonly -safe db 'select x from t'`,
		`sqlite3 -safe -readonly db 'select x from t'`,
		`/usr/bin/sqlite3 -readonly -safe /home/foci/data/api.db "SELECT * FROM api_calls LIMIT 5"`,
		`/usr/bin/sqlite3 -safe -readonly db 'select 1'`,
	} {
		assertApproval(t, ro, cmd, true)
	}
}

// The old form now prompts, and the prompt says why and what to type: an
// agent (or the human reading the prompt) learns to add -safe. Any other
// sqlite3 denial, and an ordinary no-match, carry no hint.
func TestSQLiteMissingSafeExplainsItself(t *testing.T) {
	ro := parseAutoApproveRules(CommonReadonlyRules)
	ok, reason := MatchWithEnv(ro, "Bash", bashInput(t, `sqlite3 -readonly db 'select 1'`), nil)
	if ok {
		t.Fatal("precondition: sqlite3 -readonly without -safe must not be approved")
	}
	if !strings.Contains(reason, "-safe") || !strings.Contains(reason, "writefile") {
		t.Errorf("the denial must say to add -safe and why; got %q", reason)
	}
	for _, cmd := range []string{
		`sqlite3 -readonly -safe db '.shell id'`, // has -safe: denied for the dot-command
		`sqlite3 db 'select 1'`,                  // no -readonly: not the old approved form
		`curl https://example.com`,
	} {
		if _, reason := MatchWithEnv(ro, "Bash", bashInput(t, cmd), nil); reason != "" {
			t.Errorf("%q: no -safe hint expected; got %q", cmd, reason)
		}
	}
}
