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
