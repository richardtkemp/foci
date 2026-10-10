// Tests for #2300: the shared bash JSON checks accept exactly one JSON
// value. The foci__json passthrough guard and foci__json_arg ran jq without
// -s, so jq checked each value of a multi-value stream on its own: '{}{}'
// fired the passthrough (whose own `jq --argjson` then printed jq's
// internals and called foci-call with an empty string), a key containing a
// space passed after word splitting, an empty key was dropped entirely, and
// a valid JSON prefix with trailing text ('1 x') satisfied foci__json_arg.
// The ask wrapper never checked its questions JSON at all. Every test here
// fails on the pre-#2300 helpers; the TestAsk… and TestJSONType… functions
// below were added in review and fail on the first #2300 cut (which still
// let a second positional silently overwrite the questions JSON, read
// stdin unbounded, and inlined the one-value type check).
package tools

import (
	"encoding/json"
	"fmt"
	osexec "os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// requireBashJq skips the test when bash or jq is missing — the tests here
// exec real generated bash and rely on the same jq the helpers use.
func requireBashJq(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"bash", "jq"} {
		if _, err := osexec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
}

// bashSingleQuote wraps s in single quotes for embedding in a bash -c
// script (the fixed argument vectors here contain no quotes; this keeps the
// helper honest if one ever does).
func bashSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// guardProbe runs the real foci__json helper (only jsonPassthroughHelper is
// sourced, via an empty body) against one argument and reports the helper's
// own exit status, the surviving value of the caller's __foci_json_rc
// (SENTINEL unless the helper stored a foci-call status into it, which only
// a passthrough may), the number of foci-call invocations, and the combined
// output.
func guardProbe(t *testing.T, validKeys, arg string) (guardRC int, rcVar string, calls int, out string) {
	t.Helper()
	cmd := fmt.Sprintf(
		"__foci_json_rc=SENTINEL\nfoci__json probe %s %s\necho \"GUARD_RC=$?\"\necho \"RCVAR=${__foci_json_rc:-UNSET}\"",
		bashSingleQuote(validKeys), bashSingleQuote(arg))
	calls, _, out = runCountingShellFunc(t, "", cmd, 7)
	for _, ln := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(ln, "GUARD_RC="); ok {
			guardRC, _ = strconv.Atoi(v)
		}
		if v, ok := strings.CutPrefix(ln, "RCVAR="); ok {
			rcVar = v
		}
	}
	return guardRC, rcVar, calls, out
}

// jsonArgProbe runs the real foci__json_arg helper directly and reports its
// exit status and combined output.
func jsonArgProbe(t *testing.T, flag, want, val string) (rc int, out string) {
	t.Helper()
	cmd := fmt.Sprintf("foci__json_arg %s %s %s\necho \"ARG_RC=$?\"",
		bashSingleQuote(flag), bashSingleQuote(want), bashSingleQuote(val))
	_, rc, out = runCountingShellFunc(t, "", cmd, 0)
	for _, ln := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(ln, "ARG_RC="); ok {
			rc, _ = strconv.Atoi(v)
		}
	}
	return rc, out
}

// TestJSONGuardSlurpsWholeArgumentAndComparesKeysExactly proves the two
// #2300 guard properties at the helper itself. Slurp: the guard fires only
// when the WHOLE argument is exactly one JSON value that is an object —
// concatenated objects ('{}{}', '{} {}', '{"query":"x"}{"n":1}') and
// trailing text are misses, silent, with __foci_json_rc untouched and
// foci-call not run. Exact keys: each key is compared as one whole string
// against the valid-name list, so a key containing a space ('{"query n":1}'
// against valid names query/n) or the empty string ('{"":1}' against an
// empty list) never matches — the old shell loop word-split the key list,
// so both passed. A lone object with valid (or no) keys still passes
// through, one call, storing foci-call's status.
func TestJSONGuardSlurpsWholeArgumentAndComparesKeysExactly(t *testing.T) {
	t.Parallel()
	requireBashJq(t)

	for _, c := range []struct {
		name      string
		validKeys string
		arg       string
	}{
		{"concatenated empty objects", "query n", `{}` + `{}`},
		{"concatenated with space", "query n", `{} {}`},
		{"concatenated valid-keyed objects", "query n", `{"query":"x"}{"n":1}`},
		{"trailing text", "query n", `{"query":"x"} oops`},
		{"key with a space is one key", "query n", `{"query n":1}`},
		{"empty key against empty list", "", `{"":1}`},
		{"empty key against real list", "query n", `{"":1}`},
	} {
		guardRC, rcVar, calls, out := guardProbe(t, c.validKeys, c.arg)
		if guardRC != 1 || calls != 0 || rcVar != "SENTINEL" {
			t.Errorf("%s: guard rc=%d calls=%d rcvar=%s, want rc 1, 0 calls, rcvar untouched (not a passthrough)\nout=%s",
				c.name, guardRC, calls, rcVar, out)
		}
		if want := "GUARD_RC=1\nRCVAR=SENTINEL\n"; out != want {
			t.Errorf("%s: a miss must print nothing of its own, got:\n%q", c.name, out)
		}
	}

	// Positive controls: a lone object — with valid keys, with a glob-named
	// key, or with no keys at all — is a passthrough: one foci-call, and the
	// helper stores that call's status (#2226) and returns 0.
	for _, c := range []struct {
		name      string
		validKeys string
		arg       string
		wantReq   string
	}{
		{"lone object valid keys", "query n", `{"n":1}`, `"params":{"n":1}`},
		{"lone object no keys", "query n", `{}`, `"params":{}`},
		{"glob-named key still exact-matched", "a b*", `{"b*":1}`, `"params":{"b*":1}`},
	} {
		guardRC, rcVar, calls, out := guardProbe(t, c.validKeys, c.arg)
		if guardRC != 0 || calls != 1 || rcVar != "7" {
			t.Errorf("%s: guard rc=%d calls=%d rcvar=%s, want rc 0, 1 call, rcvar 7 (passthrough)\nout=%s",
				c.name, guardRC, calls, rcVar, out)
		}
		if !strings.Contains(out, c.wantReq) {
			t.Errorf("%s: request should contain %s\nout=%s", c.name, c.wantReq, out)
		}
	}
}

// TestJSONArgSlurpsWholeValue proves foci__json_arg accepts exactly one JSON
// value (#2300). The old unslurped `jq -r 'type'` reported the FIRST
// value's type and ignored the parse error after it, so a valid prefix with
// trailing text ('1 x', '{} x') or several values ('1 2', '[1][2]') passed
// and the caller's `jq --argjson` printed its internals — the leak #1811
// added this helper to stop. Several values now report the existing
// not-JSON error (a JSON text is exactly one value), and '[1][2]' prints it
// on ONE line instead of the old garbled two-line mismatch. A lone value of
// the wanted type still passes silently.
func TestJSONArgSlurpsWholeValue(t *testing.T) {
	t.Parallel()
	requireBashJq(t)

	for _, c := range []struct {
		flag    string
		want    string
		val     string
		wantErr string
	}{
		{"--lines", "number", `1 x`, "error: --lines expects a JSON number, but this value is not JSON: 1 x"},
		{"--headers", "object", `{} x`, "error: --headers expects a JSON object, but this value is not JSON: {} x"},
		{"--watch", "any", `1 2`, "error: --watch expects a JSON any, but this value is not JSON: 1 2"},
		{"--ids", "array", `[1][2]`, "error: --ids expects a JSON array, but this value is not JSON: [1][2]"},
	} {
		rc, out := jsonArgProbe(t, c.flag, c.want, c.val)
		if rc != 1 {
			t.Errorf("%s %s %q: rc=%d, want 1\nout=%s", c.flag, c.want, c.val, rc, out)
		}
		if !strings.Contains(out, c.wantErr) {
			t.Errorf("%s %s %q: error should be the one-line not-JSON message\nout=%s", c.flag, c.want, c.val, out)
		}
		if strings.Contains(out, "argjson") || strings.Contains(out, "jq:") {
			t.Errorf("%s %s %q: jq internals leaked\nout=%s", c.flag, c.want, c.val, out)
		}
	}

	// The '[1][2]' message must be ONE line: the old check printed the
	// two-line garble "got array\narray: [1][2]".
	rc, out := jsonArgProbe(t, "--ids", "array", `[1][2]`)
	if rc != 1 || strings.Contains(out, "got array") || strings.Count(out, "error: --ids") != 1 {
		t.Errorf("--ids '[1][2]': want one not-JSON line naming --ids, got:\n%s", out)
	}

	// Positive controls: one value of the wanted type (or any type) passes.
	for _, c := range []struct {
		flag string
		want string
		val  string
	}{
		{"--lines", "number", `30`},
		{"--headers", "object", `{}`},
		{"--ids", "array", `[1]`},
		{"--watch", "any", `null`},
	} {
		rc, out := jsonArgProbe(t, c.flag, c.want, c.val)
		if rc != 0 || strings.Contains(out, "error:") {
			t.Errorf("%s %s %q: a lone value of the right type must pass, rc=%d\nout=%s", c.flag, c.want, c.val, rc, out)
		}
	}
}

// TestWhoamiConcatenatedJSONIsNotAPassthrough proves the guard fix reaches
// the zero-property body (#2271): '{}{}', '{} {}' and '{"":1}' — all of
// which the old guard accepted (no keys to invalidate, empty key dropped by
// word splitting) — now fall through to `takes no arguments` instead of
// leaking jq's --argjson internals and calling foci-call with an empty
// string.
func TestWhoamiConcatenatedJSONIsNotAPassthrough(t *testing.T) {
	t.Parallel()
	requireBashJq(t)
	assertRejectsArgs(t, generateShellFunc(NewWhoamiTool(WhoamiDeps{})), "foci_whoami", []argRejection{
		{args: []string{`{}` + `{}`}, wantErr: `foci_whoami takes no arguments, got '{}{}'`},
		{args: []string{`{} {}`}, wantErr: `foci_whoami takes no arguments, got '{} {}'`},
		{args: []string{`{"":1}`}, wantErr: `foci_whoami takes no arguments, got '{"":1}'`},
	})
}

// TestSchemaToolNoPositionalConcatenatedJSONRejected proves the guard fix
// reaches a schema-driven function with NO positional parameter: a guard
// miss falls through to the flag parser, whose bare-argument arm is the
// unexpected-positional usage error — not jq internals.
func TestSchemaToolNoPositionalConcatenatedJSONRejected(t *testing.T) {
	t.Parallel()
	requireBashJq(t)
	np := &Tool{
		Name:       "probe_np",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"},"n":{"type":"integer"}}}`),
	}
	assertRejectsArgs(t, generateShellFunc(np), "foci_probe_np", []argRejection{
		{args: []string{`{"a":"x"}{"n":1}`}, wantErr: `error: unexpected positional argument: {"a":"x"}{"n":1}`},
	})
}

// TestUnparseableSchemaConcatenatedJSONRejected proves the guard fix reaches
// the unparseable-schema fallback (#2271): its own slurp check never ran
// before, because the broken guard fired first on '{}{}' (empty valid-key
// list, no keys to check) and died inside jq --argjson. Now the guard
// declines and the #2271 body rejects the blob with its own error.
func TestUnparseableSchemaConcatenatedJSONRejected(t *testing.T) {
	t.Parallel()
	requireBashJq(t)
	blob := &Tool{
		Name:       "blob",
		ExecExport: true,
		Parameters: json.RawMessage("{"),
	}
	assertRejectsArgs(t, generateShellFunc(blob), "foci_blob", []argRejection{
		{args: []string{`{}` + `{}`}, wantErr: `error: foci_blob takes a single JSON object argument, got '{}{}'`},
	})
}

// TestPositionalToolTakesGuardMissAsPositionalText pins the documented
// fallthrough for a tool WITH a positional parameter: a guard miss means
// "not a passthrough", and the body takes the argument as the positional
// text — the same rule as any other non-passthrough JSON-looking argument
// ('{"zzz":1}'). So '{"query":"x"}{"n":1}' and '{"query n":1}' (whose key,
// compared whole, is not a valid name) each produce exactly one foci-call
// with that text as the STRING value of query, and no jq output. Before
// #2300 the guard fired on both and leaked jq's --argjson internals.
func TestPositionalToolTakesGuardMissAsPositionalText(t *testing.T) {
	t.Parallel()
	requireBashJq(t)
	withProps, _ := passthroughProbeTools()
	body := generateShellFunc(withProps)

	for _, c := range []struct {
		cmd       string
		wantQuery string
	}{
		{`foci_probe '{"query":"x"}{"n":1}'`, `"query":"{\"query\":\"x\"}{\"n\":1}"`},
		{`foci_probe '{"query n":1}'`, `"query":"{\"query n\":1}"`},
	} {
		calls, rc, out := runCountingShellFunc(t, body, c.cmd, 0)
		if calls != 1 || rc != 0 {
			t.Errorf("%s: calls=%d rc=%d, want 1 call returning 0 (normal body)\nout=%s", c.cmd, calls, rc, out)
		}
		if !strings.Contains(out, c.wantQuery) {
			t.Errorf("%s: the whole argument should travel as the positional query string %s\nout=%s", c.cmd, c.wantQuery, out)
		}
		for _, leak := range []string{"jq:", "argjson", "invalid JSON argument"} {
			if strings.Contains(out, leak) {
				t.Errorf("%s: jq internals leaked (%q)\nout=%s", c.cmd, leak, out)
			}
		}
	}
}

// TestGenericToolJSONFlagsRejectMultiValue proves the foci__json_arg fix
// through a generated generic function: a number flag fed '1 x', an object
// flag fed '{} x' and a typeless (any) flag fed '1 2' are each rejected
// with the not-JSON error naming the flag — one line, no jq internals —
// instead of passing the check and dying in the caller's jq --argjson.
func TestGenericToolJSONFlagsRejectMultiValue(t *testing.T) {
	t.Parallel()
	requireBashJq(t)
	mval := &Tool{
		Name:       "mval",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"},"m":{"type":"object"},"w":{}}}`),
	}
	assertRejectsArgs(t, generateShellFunc(mval), "foci_mval", []argRejection{
		{args: []string{"--n", "1 x"}, wantErr: "error: --n expects a JSON number, but this value is not JSON: 1 x"},
		{args: []string{"--m", "{} x"}, wantErr: "error: --m expects a JSON object, but this value is not JSON: {} x"},
		{args: []string{"--w", "1 2"}, wantErr: "error: --w expects a JSON any, but this value is not JSON: 1 2"},
	})
}

// TestTodoIDsMultiValueReportsNotJSON proves the foci__json_arg fix through
// the real todo wrapper: `--ids '[1][2]'` reports the not-JSON error on one
// line naming --ids. The old unslurped check emitted the value's type once
// per value, so the mismatch message wrapped mid-way through itself
// ("got array\narray: [1][2]").
func TestTodoIDsMultiValueReportsNotJSON(t *testing.T) {
	t.Parallel()
	requireBashJq(t)
	todo := &Tool{
		Name:       "todo",
		ExecExport: true,
		Positional: []string{"action"},
		Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"},"text":{"type":"string"},"limit":{"type":"integer"}}}`),
	}
	body := generateShellFunc(todo)
	assertRejectsArgs(t, body, "foci_todo", []argRejection{
		{args: []string{"complete", "--ids", "[1][2]"}, wantErr: "error: --ids expects a JSON array, but this value is not JSON: [1][2]"},
	})
	_, rc, out := runCountingShellFunc(t, body, `foci_todo complete --ids '[1][2]'`, 0)
	if rc != 1 || strings.Contains(out, "got array") || strings.Count(out, "error: --ids") != 1 {
		t.Errorf("todo --ids '[1][2]': want one not-JSON line naming --ids, rc 1, got rc=%d\nout=%s", rc, out)
	}
}

// TestAskRejectsNonObjectQuestionsJSON proves the ask wrapper checks its
// questions JSON before jq sees it (#2300): plain text, a non-object JSON
// value and concatenated objects each get `foci_ask expects one JSON
// object` plus the existing usage lines — no foci-call, no jq output.
// Before, the positional went straight to jq --argjson ('foo' leaked jq
// internals, '[1]' was sent as the params), and a --json value was
// validated only as `any`. The empty call keeps its existing plain usage
// message, and a valid object still sends those params once.
func TestAskRejectsNonObjectQuestionsJSON(t *testing.T) {
	t.Parallel()
	requireBashJq(t)
	ask := &Tool{
		Name:       "ask",
		ExecExport: true,
		Positional: []string{"questions"},
		Parameters: json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array"}}}`),
	}
	body := generateShellFunc(ask)

	for _, c := range []struct {
		cmd     string
		wantGot string
	}{
		{`foci_ask foo`, "got: foo"},
		{`foci_ask '[1]'`, "got: [1]"},
		{`foci_ask '{"questions":[]}{"x":1}'`, `got: {"questions":[]}{"x":1}`},
		{`foci_ask --json '[1]'`, "got: [1]"},
	} {
		calls, rc, out := runCountingShellFunc(t, body, c.cmd, 0)
		if rc != 1 || calls != 0 {
			t.Errorf("%s: rc=%d calls=%d, want rc 1 and 0 foci-calls\nout=%s", c.cmd, rc, calls, out)
		}
		if !strings.Contains(out, "error: foci_ask expects one JSON object, "+c.wantGot) {
			t.Errorf("%s: error should name the wrapper and the value\nout=%s", c.cmd, out)
		}
		if !strings.Contains(out, "usage: foci_ask") {
			t.Errorf("%s: the two usage lines must follow the error\nout=%s", c.cmd, out)
		}
		for _, leak := range []string{"jq:", "argjson", "invalid JSON argument"} {
			if strings.Contains(out, leak) {
				t.Errorf("%s: jq internals leaked (%q)\nout=%s", c.cmd, leak, out)
			}
		}
	}

	// The empty call keeps its existing plain usage message (no value to name).
	calls, rc, out := runCountingShellFunc(t, body, "foci_ask </dev/null", 0)
	if rc != 1 || calls != 0 || !strings.Contains(out, "usage: foci_ask") || strings.Contains(out, "expects one JSON object") {
		t.Errorf("bare foci_ask: want the plain usage error, rc 1, 0 calls; got rc=%d calls=%d\nout=%s", rc, calls, out)
	}

	// Positive control: a valid questions object still sends those params once.
	calls, rc, out = runCountingShellFunc(t, body, `foci_ask '{"questions":[]}'`, 3)
	if calls != 1 || rc != 3 || !strings.Contains(out, `"questions":[]`) {
		t.Errorf("valid foci_ask: want 1 call with the questions params returning 3, got calls=%d rc=%d\nout=%s", calls, rc, out)
	}
}

// jsonTypeProbe runs the real foci__json_type helper (only
// jsonPassthroughHelper is sourced) against one value and reports the
// combined output, with the helper's type answer echoed on a TYPE= line so
// an empty answer (the whole point of the check) is still visible.
func jsonTypeProbe(t *testing.T, val string) (rc int, out string) {
	t.Helper()
	cmd := fmt.Sprintf("printf 'TYPE=%%s\\n' \"$(foci__json_type %s)\"; echo RC=$?",
		bashSingleQuote(val))
	_, rc, out = runCountingShellFunc(t, "", cmd, 0)
	return rc, out
}

// TestJSONTypePrintsTypeOfExactlyOneValue pins the shared whole-value check
// foci__json_type (#2300): it prints the type of its argument ONLY when the
// whole text is exactly one JSON value — a JSON text is one value — and
// prints nothing for empty input, a parse error anywhere, or several
// values. foci__json_arg and the ask wrapper both delegate to it, so this
// is the one place the slurp semantics are pinned; before the shared
// helper existed, two near-copies of the jq filter had already drifted
// apart ("empty" output vs a "many" sentinel).
func TestJSONTypePrintsTypeOfExactlyOneValue(t *testing.T) {
	t.Parallel()
	requireBashJq(t)

	for _, c := range []struct {
		name string
		val  string
		want string
	}{
		{"object", `{}`, "object"},
		{"array", `[1]`, "array"},
		{"number", `5`, "number"},
		{"string", `"x"`, "string"},
		{"boolean", `true`, "boolean"},
		{"null", `null`, "null"},
		{"empty input", ``, ""},
		{"trailing text", `1 x`, ""},
		{"two objects", `{}` + `{}`, ""},
		{"two values with a space", `1 2`, ""},
		{"leading garbage", `x {}`, ""},
		{"parse error", `{`, ""},
	} {
		rc, out := jsonTypeProbe(t, c.val)
		if want := "TYPE=" + c.want + "\nRC=0\n"; rc != 0 || out != want {
			t.Errorf("%s: foci__json_type %q should answer exactly %q\nout=%s", c.name, c.val, c.want, out)
		}
	}
}

// TestAskTakesQuestionsJSONFromExactlyOneSource pins the one-source rule for
// the questions JSON: positional, --json and stdin are three spellings of
// ONE input, and a second spelling (a second positional, a repeated --json)
// is an error naming the clash — never a silent overwrite. Before,
// `foci_ask '{"questions":[1]}' '{"x":1}'` sent only the SECOND object, and
// `foci_ask '{"questions":[]}' foo` reported `got: foo`, hiding the dropped
// object entirely (the dropped-argument class #2271 fixed elsewhere). The
// last two arms characterise the pre-existing --json/positional clash
// errors so the whole matrix stays asserted in one place.
func TestAskTakesQuestionsJSONFromExactlyOneSource(t *testing.T) {
	t.Parallel()
	requireBashJq(t)
	ask := &Tool{
		Name:       "ask",
		ExecExport: true,
		Positional: []string{"questions"},
		Parameters: json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array"}}}`),
	}
	body := generateShellFunc(ask)

	for _, c := range []struct{ cmd, wantErr string }{
		{`foci_ask '{"questions":[1]}' '{"x":1}'`, "error: the questions JSON was already given; expected ONE questions JSON object, not several arguments"},
		{`foci_ask '{"questions":[]}' foo`, "error: the questions JSON was already given; expected ONE questions JSON object, not several arguments"},
		{`foci_ask --json '{"questions":[]}' --json '{"x":1}'`, "error: the questions JSON was already given as --json; pass the questions JSON exactly once"},
		{`foci_ask '{"questions":[]}' --json '{"x":1}'`, "error: the questions JSON was already given positionally; use --json OR the positional form, not both"},
		{`foci_ask --json '{"questions":[]}' '{"x":1}'`, "error: the questions JSON was already given as --json; use --json OR the positional form, not both"},
	} {
		calls, rc, out := runCountingShellFunc(t, body, c.cmd, 0)
		if rc != 1 || calls != 0 {
			t.Errorf("%s: rc=%d calls=%d, want rc 1 and 0 foci-calls (a source clash sends nothing)\nout=%s", c.cmd, rc, calls, out)
		}
		if !strings.Contains(out, c.wantErr) {
			t.Errorf("%s: error should name the clash\nwant: %s\nout=%s", c.cmd, c.wantErr, out)
		}
	}

	// Positive control: grader flags around the one questions JSON still work.
	calls, rc, out := runCountingShellFunc(t, body, `foci_ask '{"questions":[]}' --grader g`, 0)
	if calls != 1 || rc != 0 || !strings.Contains(out, `"grader":"g"`) || !strings.Contains(out, `"questions":[]`) {
		t.Errorf("questions JSON plus grader flags: want 1 call with both merged, got calls=%d rc=%d\nout=%s", calls, rc, out)
	}
}

// TestAskStdinReadIsBounded proves the ask wrapper's stdin read carries the
// #1552 bound the generic generator has: a pipe nobody closes cannot hang
// foci_ask. After FOCI_STDIN_WAIT the read is cut off with a stderr warning,
// the empty value falls to the usage error, and nothing is sent. Before,
// the read was a bare `cat`, so `sleep 20 | foci_ask` hung until killed.
// The blocking condition itself expires (the fifo's only writer is a sleep
// with stderr discarded), so the test asserts termination, not speed.
func TestAskStdinReadIsBounded(t *testing.T) {
	t.Parallel()
	requireBashJq(t)
	ask := &Tool{
		Name:       "ask",
		ExecExport: true,
		Positional: []string{"questions"},
		Parameters: json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array"}}}`),
	}
	body := generateShellFunc(ask)

	start := time.Now()
	calls, rc, out := runCountingShellFunc(t, body, `export FOCI_STDIN_WAIT=1
__d=$(mktemp -d); mkfifo "$__d/f"
sleep 20 > "$__d/f" 2>/dev/null &
foci_ask < "$__d/f"; __ask_rc=$?
rm -rf "$__d"
exit $__ask_rc`, 0)
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("foci_ask on a never-closing pipe took %v; the read must be bounded (#1552)", elapsed)
	}
	if rc != 1 || calls != 0 {
		t.Errorf("a cut-off read leaves an empty questions JSON: want the usage error, rc 1, 0 calls; got rc=%d calls=%d\nout=%s", rc, calls, out)
	}
	if !strings.Contains(out, "still open after") {
		t.Errorf("a cut-off read must WARN — silent truncation is worse than the hang it replaced\nout=%s", out)
	}

	// Positive control: a closing pipe still delivers the questions JSON.
	calls, rc, out = runCountingShellFunc(t, body, `echo '{"questions":[]}' | foci_ask`, 3)
	if calls != 1 || rc != 3 || !strings.Contains(out, `"questions":[]`) {
		t.Errorf("piped valid questions JSON: want 1 call returning 3, got calls=%d rc=%d\nout=%s", calls, rc, out)
	}
}
