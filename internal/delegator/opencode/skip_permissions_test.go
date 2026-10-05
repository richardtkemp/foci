package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"foci/internal/delegator"
)

// startUnattended starts a Backend against an httptest opencode that creates a
// session and records every other request, with a permission prompt func that
// records what it was asked. skipCfg sets backend_config.skip_permissions;
// skipOpt sets StartOptions.SkipPermissions (what a batch session launches with).
func startUnattended(t *testing.T, skipCfg, skipOpt bool) (*Backend, func() []permRequest, func() []string) {
	t.Helper()
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
	if skipCfg {
		b.cfg["skip_permissions"] = true
	}
	b.SetPermissionPromptFunc(func(id, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
		mu.Lock()
		prompted = append(prompted, id)
		mu.Unlock()
	})
	if err := b.Start(context.Background(), delegator.StartOptions{AgentID: "test-agent", SkipPermissions: skipOpt}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	getReqs := func() []permRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]permRequest(nil), reqs...)
	}
	getPrompted := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), prompted...)
	}
	return b, getReqs, getPrompted
}

// assertUnattendedReply checks that exactly one request went to path with the
// given JSON field value, nothing was prompted, and nothing is left pending.
func assertUnattendedReply(t *testing.T, b *Backend, reqs []permRequest, prompted []string, path, field, want string) {
	t.Helper()
	if len(prompted) != 0 {
		t.Errorf("unattended session prompted the user for %v; want no prompt", prompted)
	}
	var hits []permRequest
	for _, r := range reqs {
		if r.Path == path {
			hits = append(hits, r)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("requests to %s = %d (all: %v), want 1", path, len(hits), reqs)
	}
	var body map[string]any
	_ = json.Unmarshal(hits[0].Body, &body)
	if got, _ := body[field].(string); got != want {
		t.Errorf("%s %s = %q, want %q (body %s)", path, field, got, want, hits[0].Body)
	}
	b.permMu.Lock()
	n := len(b.pendingPerms)
	b.permMu.Unlock()
	if n != 0 {
		t.Errorf("pendingPerms has %d entries, want 0", n)
	}
	if !b.outstanding.IsEmpty() {
		t.Error("outstanding registry still holds a prompt; WaitForPermission would block")
	}
}

// TestSkipPermissions_AllowsAskedPermissionWithoutPrompt is #2153: a batch
// session launches with StartOptions.SkipPermissions and has no user to ask.
// ccstream runs it with --dangerously-skip-permissions (every tool allowed);
// opencode has no launch flag, so the backend answers each ask itself.
func TestSkipPermissions_AllowsAskedPermissionWithoutPrompt(t *testing.T) {
	for _, tc := range []struct {
		name             string
		skipCfg, skipOpt bool
	}{
		{"StartOptions", false, true},
		{"backend_config", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, reqs, prompted := startUnattended(t, tc.skipCfg, tc.skipOpt)
			b.onPermissionAsked(PermissionRequest{
				ID: "per-1", SessionID: "sess-batch", Permission: PermBash, Patterns: []string{"rm -rf build"},
			})
			assertUnattendedReply(t, b, reqs(), prompted(), "/permission/per-1/reply", "reply", "once")
		})
	}
}

// TestSkipPermissions_AllowsLegacyPermissionWithoutPrompt covers the older
// permission.updated model, answered on the per-session endpoint.
func TestSkipPermissions_AllowsLegacyPermissionWithoutPrompt(t *testing.T) {
	b, reqs, prompted := startUnattended(t, false, true)
	b.onPermissionUpdated(Permission{ID: "per-2", Type: PermEdit, Title: "edit foo.go"})
	assertUnattendedReply(t, b, reqs(), prompted(), "/session/sess-batch/permissions/per-2", "response", "allow")
}

// TestSkipPermissions_DeclinesQuestionWithoutPrompt: a question has no answer
// to allow, and nobody to give one, so it is declined (as CancelQuestion does).
func TestSkipPermissions_DeclinesQuestionWithoutPrompt(t *testing.T) {
	b, reqs, prompted := startUnattended(t, false, true)
	b.onPermissionUpdated(Permission{
		ID: "per-q", Type: PermQuestion, Title: "pick",
		Metadata: json.RawMessage(`{"text":"which?","options":[{"label":"a"}]}`),
	})
	assertUnattendedReply(t, b, reqs(), prompted(), "/session/sess-batch/permissions/per-q", "response", "deny")
}

// TestSkipPermissions_OffStillPrompts guards the other arm: an ordinary
// session must keep prompting.
func TestSkipPermissions_OffStillPrompts(t *testing.T) {
	b, reqs, prompted := startUnattended(t, false, false)
	b.onPermissionAsked(PermissionRequest{
		ID: "per-3", SessionID: "sess-batch", Permission: PermBash, Patterns: []string{"rm -rf build"},
	})
	if got := prompted(); len(got) != 1 || got[0] != "per-3" {
		t.Errorf("prompted = %v, want [per-3]", got)
	}
	for _, r := range reqs() {
		if strings.Contains(r.Path, "per-3") {
			t.Errorf("unexpected reply %s %s for an attended session", r.Path, r.Body)
		}
	}
}
