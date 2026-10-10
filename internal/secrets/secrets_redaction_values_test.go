package secrets

import (
	"testing"
)

// TestStoreRedactionValues pins the contract telemetry's live trace
// redaction is built on (#2249): the root store's RedactionValues returns
// every value in the file — the globals AND the [agents.<id>.*] override
// tables, which Names() deliberately excludes for the root — from one live
// snapshot per call, so a value added to the file after Load is returned by
// the very next call. A per-agent view must not gain values it cannot see:
// another agent's override never appears.
func TestStoreRedactionValues(t *testing.T) {
	path := writeSecrets(t, `
[custom]
api_key = "globalval01"

[agents.worker.custom]
api_key = "agentover01"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	has := func(vals []string, want string) bool {
		for _, v := range vals {
			if v == want {
				return true
			}
		}
		return false
	}

	root := s.RedactionValues()
	if !has(root, "globalval01") {
		t.Errorf("root RedactionValues() = %q — missing the global value of custom.api_key", root)
	}
	if !has(root, "agentover01") {
		t.Errorf("root RedactionValues() = %q — missing the override value of agents.worker.custom.api_key", root)
	}

	// The slice is fresh: appending to it must not corrupt the shared state.
	root = append(root, "mutated99")
	if again := s.RedactionValues(); has(again, "mutated99") {
		t.Error("RedactionValues() aliased shared state — a caller's append reached the next call")
	}

	// A value added to the file after Load is returned by the very next
	// call: one live current() snapshot per call, no restart.
	next := mtimeSeq()
	rewrite(t, path, "[custom]\napi_key = \"globalval01\"\n\n[extra]\nlive = \"lateadded01\"\n\n[agents.worker.custom]\napi_key = \"agentover01\"\n", next())
	live := s.RedactionValues()
	if !has(live, "lateadded01") {
		t.Errorf("root RedactionValues() after the file changed = %q — missing extra.live; the read was not live", live)
	}

	// A view never gains another agent's override, but keeps the globals it
	// can see.
	other := s.ForAgent("other").RedactionValues()
	if has(other, "agentover01") {
		t.Errorf("view RedactionValues() = %q — leaks agents.worker.custom.api_key, a value the view cannot see", other)
	}
	if !has(other, "globalval01") {
		t.Errorf("view RedactionValues() = %q — missing the unrestricted global custom.api_key", other)
	}

	// Store.Redact is untouched by this change: it keeps scrubbing exactly
	// valuesFor's set — for the root the globals, NOT the agent tables.
	if out := s.Redact("x agentover01 globalval01 y"); out != "x agentover01 [REDACTED] y" {
		t.Errorf("root Redact() = %q — its valuesFor-derived behaviour must not change", out)
	}
}
