package opencode

import (
	"context"
	"io"
	"net/http"
	"sync"
	"testing"

	"foci/internal/delegator"
	"foci/internal/delegator/autoapprove"
)

// TestReadOnlyBatch_BashDecidedByReadOnlyRulesOnly is #2702 on opencode: a
// read-only batch (spawn explore) asks about every bash command, and foci
// answers from the read-only rules alone — a whitelisted read runs, anything
// else is rejected at once. Never prompted, even when the session was not
// started unattended (a spawn whose other asks would go up to its spawner).
func TestReadOnlyBatch_BashDecidedByReadOnlyRulesOnly(t *testing.T) {
	for _, tc := range []struct {
		name, command, want string
	}{
		{"whitelisted read runs", "jq .a /tmp/f.json", "once"},
		{"sqlite read-only query runs", "sqlite3 -readonly /tmp/x.db 'select 1'", "once"},
		{"write command rejected", "rm -rf build", "reject"},
		{"write flag of a listed command rejected", "sed -i s/a/b/ f.txt", "reject"},
		{"redirect to a file rejected", "jq . f.json > out.json", "reject"},
		{"sqlite dot-command rejected", "sqlite3 -readonly :memory: '.shell touch /tmp/pwned'", "reject"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var reqs []permRequest
			var prompted []string
			_, b := newTestBackendServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/session" && r.Method == http.MethodPost {
					_, _ = w.Write([]byte(`{"id":"sess-batch"}`))
					return
				}
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				reqs = append(reqs, permRequest{Path: r.URL.Path, Body: body})
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
			})
			b.pendingPerms = make(map[string]*pendingPermission)
			b.cfg = map[string]any{}
			b.SetPermissionPromptFunc(func(id, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
				mu.Lock()
				prompted = append(prompted, id)
				mu.Unlock()
			})
			if err := b.Start(context.Background(), delegator.StartOptions{
				AgentID:          "test-agent",
				BatchAccess:      delegator.BatchAccessReadOnly,
				AutoApproveRules: autoapprove.CommonReadonlyRules,
			}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() { _ = b.Close() })

			// Only the reply this test provokes counts; Start's ruleset PATCH
			// is not a permission reply.
			mu.Lock()
			reqs = nil
			mu.Unlock()
			b.onPermissionAsked(PermissionRequest{
				ID: "per-1", SessionID: "sess-batch", Permission: PermBash, Patterns: []string{tc.command},
			})
			mu.Lock()
			gotReqs, gotPrompted := append([]permRequest(nil), reqs...), append([]string(nil), prompted...)
			mu.Unlock()
			assertUnattendedReply(t, b, gotReqs, gotPrompted, "/permission/per-1/reply", "reply", tc.want)
		})
	}
}
