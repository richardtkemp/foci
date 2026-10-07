package tools

import (
	"encoding/json"
	"fmt"
	osexec "os/exec"
	"strings"
	"testing"
)

// passthroughProbeTools returns one generated function with schema properties
// (query positional + n) and one zero-property fallback function, the two
// shapes every guard change must keep honest.
func passthroughProbeTools() (withProps *Tool, zeroProps *Tool) {
	withProps = &Tool{
		Name: "probe", ExecExport: true, Positional: []string{"query"},
		Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"n":{"type":"integer"}}}`),
	}
	zeroProps = &Tool{
		Name:       "probe0",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
	}
	return
}

// runCountingShellFunc runs cmd against a generated function with the real
// foci__json helper and a counting stub foci-call that prints one FCALL| line
// per invocation (the request it was given) and exits with the chosen status.
// The count is read from the output, so it survives a set -e caller exiting
// mid-script on a failing status.
func runCountingShellFunc(t *testing.T, body, cmd string, status int) (calls, rc int, out string) {
	t.Helper()
	script := jsonPassthroughHelper +
		fmt.Sprintf("foci-call() { echo \"FCALL|$1\"; return %d; }\n", status) +
		body + "\n" + cmd + "\n"
	c := osexec.Command("bash", "-c", script)
	outb, err := c.CombinedOutput()
	if ee, ok := err.(*osexec.ExitError); ok {
		rc = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("bash: %v", err)
	}
	out = string(outb)
	calls = strings.Count(out, "FCALL|")
	return calls, rc, out
}

// TestShellFuncPassthroughCallsFociCallOnce proves that when the JSON
// passthrough guard fires (a lone JSON-object argument with valid keys), the
// generated function calls foci-call EXACTLY once and returns its exit status
// — whatever that status is (#2226). The old guard fell through to the normal
// body whenever the passthrough foci-call failed, running foci-call a second
// time — twice the side effect for wrappers like foci_send_to_chat — so the
// failing statuses (1, 3) are red on the old code. Both generated shapes are
// covered (schema properties and the zero-property fallback), plus one run
// each under set -u and set -e: the guard's status slot must be declared
// before use, and a failing passthrough must propagate out like the normal
// body's trailing foci-call always has, not abort the caller mid-function.
// The leak-check case also proves __foci_json_rc stays function-local: a
// caller-set value survives a passthrough call unchanged (the guard's local
// catches the helper's dynamic-scope assignment).
func TestShellFuncPassthroughCallsFociCallOnce(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"bash", "jq"} {
		if _, err := osexec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	withProps, zeroProps := passthroughProbeTools()

	cases := []struct {
		name    string
		tool    *Tool
		cmd     string
		status  int
		wantOut string // extra substring the output must contain ("" = none)
	}{
		{"props status 0", withProps, `foci_probe '{"query":"x","n":1}'`, 0, ""},
		{"props status 1", withProps, `foci_probe '{"query":"x","n":1}'`, 1, ""},
		{"props status 3", withProps, `foci_probe '{"query":"x","n":1}'`, 3, ""},
		{"zero-props status 0", zeroProps, `foci_probe0 '{}'`, 0, ""},
		{"zero-props status 1", zeroProps, `foci_probe0 '{}'`, 1, ""},
		{"zero-props status 3", zeroProps, `foci_probe0 '{}'`, 3, ""},
		{"props status 1 under set -u", withProps, `set -u; foci_probe '{"query":"x"}'`, 1, ""},
		{"props status 1 under set -e", withProps, `set -e; foci_probe '{"query":"x"}'`, 1, ""},
		{"props caller value not clobbered", withProps, `__foci_json_rc=SENTINEL; foci_probe '{"query":"x"}'; echo "RCVAR=${__foci_json_rc:-UNSET}"`, 0, "RCVAR=SENTINEL"},
	}
	for _, c := range cases {
		calls, rc, out := runCountingShellFunc(t, generateShellFunc(c.tool), c.cmd, c.status)
		if calls != 1 {
			t.Errorf("%s: foci-call ran %d times, want exactly 1\nout=%s", c.name, calls, out)
		}
		if rc != c.status {
			t.Errorf("%s: rc=%d, want the foci-call status %d\nout=%s", c.name, rc, c.status, out)
		}
		if c.wantOut != "" && !strings.Contains(out, c.wantOut) {
			t.Errorf("%s: output lacks %q — __foci_json_rc leaked into the caller\nout=%s", c.name, c.wantOut, out)
		}
	}
}

// TestShellFuncPassthroughMissRunsNormalBody pins the guard's miss path: a
// positional (non-JSON) argument is not a passthrough, so the function's
// normal body runs and builds the params itself (query from the positional).
// Exactly one foci-call, with the parsed value, and the body's foci-call
// status returned. Characterisation: this holds on the pre-#2226 guard too;
// the test locks that the single-call change did not narrow the guard.
func TestShellFuncPassthroughMissRunsNormalBody(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"bash", "jq"} {
		if _, err := osexec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	withProps, _ := passthroughProbeTools()

	for _, status := range []int{0, 1} {
		calls, rc, out := runCountingShellFunc(t, generateShellFunc(withProps), `foci_probe hello`, status)
		if calls != 1 {
			t.Errorf("status %d: foci-call ran %d times, want 1 (normal body)\nout=%s", status, calls, out)
		}
		if !strings.Contains(out, `"query":"hello"`) {
			t.Errorf("status %d: normal body did not parse the positional into query\nout=%s", status, out)
		}
		if rc != status {
			t.Errorf("status %d: rc=%d, want the body's foci-call status\nout=%s", status, rc, out)
		}
	}
}
