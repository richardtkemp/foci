package session

import "testing"

// TestBranchBackendLifetime covers the agreed conversation/worker split, not reset-hook flags.
func TestBranchBackendLifetime(t *testing.T) {
	for _, kind := range []string{"reflection", "keepalive", "background", "consolidation", "spawn", "branch", "compaction-memory", "nudge-extraction"} {
		o := BranchOptions{BranchType: kind}
		if !o.BackendEphemeral() {
			t.Errorf("%s should be ephemeral", kind)
		}
		o.Persistent = true
		if o.BackendEphemeral() {
			t.Errorf("%s explicit persistence ignored", kind)
		}
	}
	for _, o := range []BranchOptions{{BranchType: "depth"}, {BranchType: "facet"}, {BranchType: "fork"}, {BranchType: "session-end-memory"}, {BranchType: "spawn", Fresh: true}, {BranchType: "unknown", NoResetHook: true}} {
		if o.BackendEphemeral() {
			t.Errorf("%+v should be persistent", o)
		}
	}
}
