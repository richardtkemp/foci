package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runShellFunc sources the generated bash for one tool (plus the shared helper
// preamble) in a FRESH bash, stubs foci-call so nothing reaches the socket, and
// runs the given invocation. It returns combined output and the exit code.
//
// The harness deliberately does three things #1778 proved necessary: it writes
// the generated text to a file and runs `bash -n` on it FIRST (a syntax error in
// generated bash otherwise surfaces as a silent no-op), it checks that `source`
// itself succeeded, and it runs in a fresh bash so a live foci-injected function
// of the same name cannot shadow the one under test.
func runShellFunc(t *testing.T, body, invocation string) (string, int) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "funcs.sh")
	if err := os.WriteFile(path, []byte(jsonPassthroughHelper+body), 0o600); err != nil {
		t.Fatalf("write generated funcs: %v", err)
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("generated bash is not syntactically valid: %v\n%s", err, out)
	}
	script := "set -u\n" +
		"source " + path + " || { echo 'SOURCE FAILED' >&2; exit 99; }\n" +
		"foci-call() { echo \"FOCI_CALL_REACHED:$1\"; return 0; }\n" +
		invocation + "\n"
	cmd := exec.Command("bash", "-c", script)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run generated func: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "SOURCE FAILED") {
		t.Fatalf("sourcing the generated funcs failed:\n%s", out)
	}
	return string(out), code
}

func httpToolForShellTest() *Tool {
	return NewHTTPRequestTool(nil, nil, "", func() int { return 0 }, func() int64 { return 0 }, func() int64 { return 0 }, nil, 0640)
}

// TestJSONFlag_NonJSONValueNamesTheFlag is the #1811 repro. Before the fix the
// caller saw jq's "invalid JSON text passed to --argjson" TWICE and neither line
// named --headers or the offending value.
func TestJSONFlag_NonJSONValueNamesTheFlag(t *testing.T) {
	t.Parallel()
	body := generateShellFunc(httpToolForShellTest())
	out, code := runShellFunc(t, body, `foci_http_request https://example.com --headers "Authorization=Bearer x"`)

	if code == 0 {
		t.Errorf("expected a non-zero exit for a non-JSON --headers value, got 0\n%s", out)
	}
	// The hint line is asserted too. Without it the type-mismatch branch alone
	// still rejects an unparseable value (got is empty, which != "object"), so a
	// fail-arm deleting the not-JSON branch went undetected — found by the
	// fail-arm matrix, 2026-09-01.
	hint := `e.g. --headers '{"Authorization":"Bearer TOKEN"}'`
	for _, want := range []string{"--headers", "JSON object", "Authorization=Bearer x", hint} {
		if !strings.Contains(out, want) {
			t.Errorf("error message does not mention %q\ngot:\n%s", want, out)
		}
	}
	if strings.Contains(out, "argjson") {
		t.Errorf("raw jq --argjson internals leaked to the caller\ngot:\n%s", out)
	}
	if strings.Contains(out, "FOCI_CALL_REACHED") {
		t.Errorf("a rejected value still reached foci-call\ngot:\n%s", out)
	}
}

// TestJSONFlag_WrongJSONTypeIsRejected proves valid JSON of the wrong shape is
// caught too — an array where an object is required would otherwise travel all
// the way to the server before failing.
func TestJSONFlag_WrongJSONTypeIsRejected(t *testing.T) {
	t.Parallel()
	body := generateShellFunc(httpToolForShellTest())
	out, code := runShellFunc(t, body, `foci_http_request https://example.com --headers '["a","b"]'`)

	if code == 0 {
		t.Errorf("expected a non-zero exit for an array passed to --headers, got 0\n%s", out)
	}
	if !strings.Contains(out, "--headers") || !strings.Contains(out, "got array") {
		t.Errorf("expected an error naming --headers and the actual type, got:\n%s", out)
	}
}

// TestJSONFlag_ValidValueStillReachesTheCall is the positive control: without it
// a validator that rejected everything would pass both tests above.
func TestJSONFlag_ValidValueStillReachesTheCall(t *testing.T) {
	t.Parallel()
	body := generateShellFunc(httpToolForShellTest())
	out, code := runShellFunc(t, body, `foci_http_request https://example.com --headers '{"Authorization":"Bearer x"}'`)

	if code != 0 {
		t.Errorf("valid JSON --headers should succeed, got exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "FOCI_CALL_REACHED") {
		t.Errorf("valid JSON --headers did not reach foci-call\ngot:\n%s", out)
	}
	if strings.Contains(out, "error:") {
		t.Errorf("valid JSON --headers produced an error\ngot:\n%s", out)
	}
}

// TestGenericGenerator_EveryArgjsonIsValidatedFirst pins the invariant rather
// than one tool: in schema-driven generated bash, every jq --argjson that takes
// a caller-supplied flag value must be preceded by a foci__json_arg check. A new
// tool with an object param added later cannot silently reintroduce #1811.
func TestGenericGenerator_EveryArgjsonIsValidatedFirst(t *testing.T) {
	t.Parallel()
	tool := &Tool{
		Name:        "shapecheck",
		Description: "fixture tool exercising every JSON-valued param type",
		ExecExport:  true,
		Parameters: []byte(`{"type":"object","properties":{
			"note":{"type":"string"},
			"flagged":{"type":"boolean"},
			"count":{"type":"integer"},
			"ratio":{"type":"number"},
			"mapping":{"type":"object"},
			"items":{"type":"array"}
		}}`),
	}
	body := generateGenericShellFunc(tool)

	// The trailing foci-call passes $params, which the function built itself —
	// not a caller value — so it is excluded.
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "jq --argjson v ") {
			continue
		}
		if i == 0 || !strings.Contains(lines[i-1], "foci__json_arg --") {
			t.Errorf("line %d takes a caller value with --argjson but is not preceded by a foci__json_arg check:\n%s", i+1, line)
		}
	}
	for _, want := range []string{
		"foci__json_arg --count number",
		"foci__json_arg --ratio number",
		"foci__json_arg --mapping object",
		"foci__json_arg --items array",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("generated body is missing %q\n---\n%s", want, body)
		}
	}
	// Strings and booleans do not go through --argjson and must not be checked.
	for _, unwanted := range []string{"foci__json_arg --note", "foci__json_arg --flagged"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("non-JSON param wrongly validated as JSON: %q\n---\n%s", unwanted, body)
		}
	}
}
