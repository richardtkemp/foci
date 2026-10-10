package tools

import (
	"encoding/json"
	osexec "os/exec"
	"strings"
	"testing"
)

// argRejection is one argument vector fed to a generated function and the
// exact error substring the rejection must print (#2271): it names the tool
// and the first offending argument.
type argRejection struct {
	args    []string
	wantErr string
}

// assertRejectsArgs feeds each case to the generated function through
// runCountingShellFunc (real jsonPassthroughHelper, counting stub foci-call)
// and asserts the #2271 rejection contract: rc 1, foci-call never ran, the
// error contains wantErr, and no jq internals leaked ("jq:", "argjson",
// "invalid JSON argument" — what the old unchecked `jq --argjson` fallback
// body printed instead of a usage error naming the tool).
func assertRejectsArgs(t *testing.T, body, fnName string, cases []argRejection) {
	t.Helper()
	for _, c := range cases {
		quoted := make([]string, len(c.args))
		for i, a := range c.args {
			quoted[i] = "'" + a + "'"
		}
		cmd := fnName + " " + strings.Join(quoted, " ")
		calls, rc, out := runCountingShellFunc(t, body, cmd, 0)
		if rc != 1 {
			t.Errorf("%s: rc=%d, want 1 (usage error)\nout=%s", cmd, rc, out)
		}
		if calls != 0 {
			t.Errorf("%s: foci-call ran %d times, want 0 on a rejected call\nout=%s", cmd, calls, out)
		}
		if !strings.Contains(out, c.wantErr) {
			t.Errorf("%s: error should contain %q\nout=%s", cmd, c.wantErr, out)
		}
		for _, leak := range []string{"jq:", "argjson", "invalid JSON argument"} {
			if strings.Contains(out, leak) {
				t.Errorf("%s: jq internals leaked (%q) instead of a usage error\nout=%s", cmd, leak, out)
			}
		}
	}
}

// TestShellFuncWhoamiRejectsAllArguments proves the real foci_whoami (the
// only zero-property ExecExport tool) rejects EVERY argument left after the
// prologue with a usage error naming the tool and the first argument (#2271).
// The old fallback body handed $1 to jq --argjson unchecked — `foci_whoami
// foo` printed jq's internals plus "invalid JSON argument", `'' foo`
// silently DROPPED foo (`set -- '{}'` on an empty $1 replaces all args),
// `'{}' x` ignored x, and `[1]`/`"x"`/`5`/`{"a":1}` passed through as the
// params. The empty string is an error too, on purpose: "any argument is an
// error", like a schema tool's unexpected positional — special-casing ''
// would bring back the dropped-argument bug.
func TestShellFuncWhoamiRejectsAllArguments(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"bash", "jq"} {
		if _, err := osexec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	body := generateShellFunc(NewWhoamiTool(WhoamiDeps{}))
	assertRejectsArgs(t, body, "foci_whoami", []argRejection{
		{args: []string{"foo"}, wantErr: "foci_whoami takes no arguments, got 'foo'"},
		{args: []string{"--json", "foo"}, wantErr: "foci_whoami takes no arguments, got 'foo'"},
		{args: []string{"{"}, wantErr: "foci_whoami takes no arguments, got '{'"},
		{args: []string{""}, wantErr: "foci_whoami takes no arguments, got ''"},
		{args: []string{"", "foo"}, wantErr: "foci_whoami takes no arguments, got ''"},
		{args: []string{"{}", "x"}, wantErr: "foci_whoami takes no arguments, got '{}'"},
		{args: []string{"[1]"}, wantErr: "foci_whoami takes no arguments, got '[1]'"},
		{args: []string{`"x"`}, wantErr: `foci_whoami takes no arguments, got '"x"'`},
		{args: []string{"5"}, wantErr: "foci_whoami takes no arguments, got '5'"},
		{args: []string{`{"a":1}`}, wantErr: `foci_whoami takes no arguments, got '{"a":1}'`},
		{args: []string{"--foo"}, wantErr: "foci_whoami takes no arguments, got '--foo'"},
	})
}

// TestShellFuncZeroPropertyRejectsAllArguments re-proves the no-arguments
// rule on a synthetic zero-property tool, so it is pinned on the GENERATOR
// (any tool whose schema parses with no properties), not on the whoami tool
// name. The four cases are the distinct failure shapes of the old body:
// plain junk, the dropped-extra-argument bug, non-object JSON and an object
// with keys the schema cannot have.
func TestShellFuncZeroPropertyRejectsAllArguments(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"bash", "jq"} {
		if _, err := osexec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	_, zeroProps := passthroughProbeTools()
	assertRejectsArgs(t, generateShellFunc(zeroProps), "foci_probe0", []argRejection{
		{args: []string{"foo"}, wantErr: "foci_probe0 takes no arguments, got 'foo'"},
		{args: []string{"", "foo"}, wantErr: "foci_probe0 takes no arguments, got ''"},
		{args: []string{"[1]"}, wantErr: "foci_probe0 takes no arguments, got '[1]'"},
		{args: []string{`{"a":1}`}, wantErr: `foci_probe0 takes no arguments, got '{"a":1}'`},
	})
}

// TestShellFuncZeroPropertyUsageLineJSONFlag pins the rejection's usage line:
// `usage: foci_<name> [--json]`, with " [--json]" dropped for a tool that
// does not have the --json OUTPUT flag (hasJSONOutputFlag — an input-flag
// tool must not advertise --json as a free-standing flag). Checked on the
// generated body because the line only exists after a rejected call, and no
// production tool has the flag-less shape today.
func TestShellFuncZeroPropertyUsageLineJSONFlag(t *testing.T) {
	t.Parallel()
	zp := &Tool{
		Name:       "zp",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
	}
	body := generateGenericShellFunc(zp)
	if !strings.Contains(body, `takes no arguments, got '$1'`) {
		t.Errorf("zero-property body should reject arguments naming the first one:\n%s", body)
	}
	if !strings.Contains(body, `echo "usage: foci_zp [--json]" >&2`) {
		t.Errorf("zero-property usage line should advertise [--json] for a --json-output tool:\n%s", body)
	}

	// "ask" is the name whose --json is an INPUT flag (shellJSONInputTools),
	// so its usage line must not offer a bare [--json].
	askLike := &Tool{
		Name:       "ask",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
	}
	body = generateGenericShellFunc(askLike)
	if !strings.Contains(body, `echo "usage: foci_ask" >&2`) {
		t.Errorf("flag-less tool usage line should be exactly the function name:\n%s", body)
	}
	if strings.Contains(body, "[--json]") {
		t.Errorf("a tool without the --json output flag must not advertise [--json]:\n%s", body)
	}
}

// TestShellFuncUnparseableSchemaRejectsBadArguments proves the legacy
// JSON-blob contract for a tool whose schema does not parse: one JSON object
// at most. More than one argument, a non-JSON argument, or JSON that is not
// an object is rejected with `error: foci_<name>`, naming the offender, no
// jq output, rc 1, and no foci-call. Before #2271 the blob went to
// `jq --argjson` unchecked, so `foo` leaked jq's internals and `[1]` was
// sent to the gateway as the params. `{"a":1} x` and `{"a":1}{"b":2}` pin
// that the check proves the WHOLE argument is one object: unslurped
// `jq -r 'type'` emits `object` per parsed value and nothing else, so
// trailing garbage fails jq only AFTER the first value's output exists (an
// output-only check sees `object` and waves `{"a":1} x` through), while
// `{"a":1}{"b":2}` was caught only by the accident of a second output line.
// The slurped check (`length==1`) rejects both by construction.
func TestShellFuncUnparseableSchemaRejectsBadArguments(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"bash", "jq"} {
		if _, err := osexec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	blob := &Tool{
		Name:       "blob",
		ExecExport: true,
		Parameters: json.RawMessage("{"),
	}
	assertRejectsArgs(t, generateShellFunc(blob), "foci_blob", []argRejection{
		{args: []string{"foo"}, wantErr: "error: foci_blob takes a single JSON object argument, got 'foo'"},
		{args: []string{""}, wantErr: "error: foci_blob takes a single JSON object argument, got ''"},
		{args: []string{"[1]"}, wantErr: "error: foci_blob takes a single JSON object argument, got '[1]'"},
		{args: []string{"{}", "{}"}, wantErr: "error: foci_blob takes a single JSON object argument, got an extra argument '{}'"},
		{args: []string{`{"a":1} x`}, wantErr: `error: foci_blob takes a single JSON object argument, got '{"a":1} x'`},
		{args: []string{`{"a":1}{"b":2}`}, wantErr: `error: foci_blob takes a single JSON object argument, got '{"a":1}{"b":2}'`},
	})
}

// TestShellFuncUnparseableSchemaSuccessCalls characterises the kept half of
// the legacy JSON-blob contract for an unparseable schema: a bare call sends
// params {} and one lone JSON object (any keys — there is no schema to check
// them against) is sent as the params, exactly one foci-call, returning its
// status. Both already worked before #2271; this test pins that the new
// checks did not narrow them.
func TestShellFuncUnparseableSchemaSuccessCalls(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"bash", "jq"} {
		if _, err := osexec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	blob := &Tool{
		Name:       "blob",
		ExecExport: true,
		Parameters: json.RawMessage("{"),
	}
	body := generateShellFunc(blob)
	cases := []struct {
		cmd      string
		wantReqs []string
	}{
		{cmd: "foci_blob", wantReqs: []string{`"params":{}`}},
		{cmd: `foci_blob '{"k":1}'`, wantReqs: []string{`"params":{"k":1}`}},
	}
	for _, c := range cases {
		calls, rc, out := runCountingShellFunc(t, body, c.cmd, 3)
		if calls != 1 {
			t.Errorf("%s: foci-call ran %d times, want 1\nout=%s", c.cmd, calls, out)
		}
		for _, want := range c.wantReqs {
			if !strings.Contains(out, want) {
				t.Errorf("%s: request should contain %s\nout=%s", c.cmd, want, out)
			}
		}
		if rc != 3 {
			t.Errorf("%s: rc=%d, want the foci-call status 3\nout=%s", c.cmd, rc, out)
		}
	}
}

// TestShellFuncZeroPropertyUnchangedCalls characterises the zero-property
// calls #2271 must not break: bare, --json, '{}' and --json '{}' each call
// foci-call exactly once with params {} and return its status, and -h still
// prints the help. All four worked before #2271 (the {} forms go through
// the foci__json passthrough guard); this test pins that the new argument
// check did not narrow them.
func TestShellFuncZeroPropertyUnchangedCalls(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"bash", "jq"} {
		if _, err := osexec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	_, zeroProps := passthroughProbeTools()
	body := generateShellFunc(zeroProps)
	for _, cmd := range []string{"foci_probe0", "foci_probe0 --json", `foci_probe0 '{}'`, `foci_probe0 --json '{}'`} {
		calls, rc, out := runCountingShellFunc(t, body, cmd, 3)
		if calls != 1 {
			t.Errorf("%s: foci-call ran %d times, want 1\nout=%s", cmd, calls, out)
		}
		if !strings.Contains(out, `"params":{}`) {
			t.Errorf("%s: should send params {}\nout=%s", cmd, out)
		}
		if rc != 3 {
			t.Errorf("%s: rc=%d, want the foci-call status 3\nout=%s", cmd, rc, out)
		}
	}

	calls, rc, out := runCountingShellFunc(t, body, "foci_probe0 -h", 3)
	if rc != 0 || calls != 0 || !strings.Contains(out, "--json (flag, any position)") {
		t.Errorf("foci_probe0 -h: want rc 0, 0 calls and the help text, got rc=%d calls=%d\nout=%s", rc, calls, out)
	}
}
