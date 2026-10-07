package tools

import (
	"encoding/json"
	osexec "os/exec"
	"strings"
	"testing"
)

// TestShellFuncZeroPropertySchemaBareCall proves a generated zero-property
// function is callable BARE (the expected usage of foci_whoami): the
// JSON-blob fallback body defaults an absent $1 to {}, so the stubbed
// foci-call receives params:{} — not the jq --argjson failure that an empty
// "$1" produced before #1135. The --json form must compose the same way
// (the strip empties the args, the default restores the blob).
func TestShellFuncZeroPropertySchemaBareCall(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"bash", "jq"} {
		if _, err := osexec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	tool := &Tool{
		Name:       "probe0",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
	}
	rc, out := runStubbedShellFunc(t, generateShellFunc(tool), "foci_probe0")
	if rc != 0 {
		t.Errorf("bare foci_probe0 rc=%d out=%q, want 0", rc, out)
	}
	if !strings.Contains(out, `"params":{}`) {
		t.Errorf("bare call should send params:{}, got %q", out)
	}

	rc, out = runStubbedShellFunc(t, generateShellFunc(tool), "foci_probe0 --json")
	if rc != 0 {
		t.Errorf("bare foci_probe0 --json rc=%d out=%q, want 0", rc, out)
	}
	if !strings.Contains(out, "fmt=json") || !strings.Contains(out, `"params":{}`) {
		t.Errorf("bare --json call should send fmt=json and params:{}, got %q", out)
	}
}

// TestShellFuncZeroPropertySchemaBareCallStrictStub re-proves the bare-call
// default of TestShellFuncZeroPropertySchemaBareCall against a STRICT stub
// foci-call that exits non-zero unless it receives a JSON request whose
// params is an object (#2226). The lenient stub in that test always exits 0,
// so its `rc != 0` checks could never fire; here rc 0 means the request was
// actually well-formed. Characterisation: the `set -- '{}'` fallback in the
// zero-property body already produces params:{} today — remove that line and
// this test's rc assertion fails.
func TestShellFuncZeroPropertySchemaBareCallStrictStub(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"bash", "jq"} {
		if _, err := osexec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	tool := &Tool{
		Name:       "probe0",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
	}
	for _, cmd := range []string{"foci_probe0", "foci_probe0 --json"} {
		rc, out := runStrictStubShellFunc(t, generateShellFunc(tool), cmd)
		if rc != 0 {
			t.Errorf("%s through strict stub: rc=%d, want 0 (params must be an object)\nout=%q", cmd, rc, out)
		}
		if !strings.Contains(out, `"params":{}`) {
			t.Errorf("%s through strict stub: should send params:{}, got %q", cmd, out)
		}
	}
}
