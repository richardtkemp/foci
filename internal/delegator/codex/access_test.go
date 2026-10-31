package codex

import (
	"encoding/json"
	"testing"

	"foci/internal/delegator"
)

// TestStartThread_ReadOnlyBatchSandbox proves a read-only batch session (spawn
// explore) starts its thread in codex's read-only sandbox even when the agent
// is configured for a writable one, and that any other session keeps the
// agent's sandbox.
func TestStartThread_ReadOnlyBatchSandbox(t *testing.T) {
	for _, c := range []struct {
		access delegator.BatchAccess
		want   string
	}{
		{delegator.BatchAccessReadOnly, "read-only"},
		{delegator.BatchAccessParent, "danger-full-access"},
		{delegator.BatchAccessDefault, "danger-full-access"},
	} {
		t.Run(c.access.String(), func(t *testing.T) {
			var got string
			b := newStartableBackend(t, func(method string, params json.RawMessage, id int64) (json.RawMessage, error) {
				var p threadStartParams
				if err := json.Unmarshal(params, &p); err != nil {
					t.Fatalf("unmarshal thread/start params: %v", err)
				}
				got = p.Sandbox
				return json.RawMessage(`{"approvalsReviewer":"auto_review","thread":{"id":"th_ro"}}`), nil
			})
			b.cfg = map[string]any{"sandbox": "danger-full-access"}
			b.workDir = "/tmp"
			b.startOpts = delegator.StartOptions{BatchAccess: c.access}
			if _, err := b.startThread(); err != nil {
				t.Fatalf("startThread: %v", err)
			}
			if got != c.want {
				t.Errorf("sandbox = %q, want %q", got, c.want)
			}
		})
	}
}
