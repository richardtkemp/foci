package opencode

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"foci/internal/delegator"
)

// The fixtures below are the live opencode 1.17.15 wire shapes, captured from a
// throwaway `opencode serve` on 2026-11-02 (#2751):
//
//   - A reply to a request opencode no longer holds returns
//     HTTP 404 {"_tag":"PermissionNotFoundError","requestID":...,"message":...}.
//     A "reject" or "always" reply settles every sibling ask in the session that
//     it covers, so the dedup aliases foci fans the same decision out to are
//     already gone by the time their POST lands.
//   - Each settled request publishes
//     permission.replied {"sessionID","requestID","reply"} — not the legacy
//     {"permissionID","response"} shape.

// TestRespondToPermission_AliasAlreadySettledIsNotAnError proves a 404 on an
// alias (opencode already settled it via the primary's cascade) counts as
// settled: no error back to the caller, and nothing left waiting.
func TestRespondToPermission_AliasAlreadySettledIsNotAnError(t *testing.T) {
	b, _ := newPermTestBackend(t)
	// opencode answers the primary, then 404s the alias it cascaded.
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/per-B/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"_tag":"PermissionNotFoundError","requestID":"per-B","message":"Permission request not found: per-B"}`))
			return
		}
		_, _ = w.Write([]byte(`true`))
	}))
	t.Cleanup(hs.Close)
	b.server = &Server{baseURL: hs.URL, http: hs.Client(), agentID: "perm-test"}

	target := PermissionRequest{Permission: PermBash, Patterns: []string{"echo same"}, SessionID: "sess-perm"}
	a, bb := target, target
	a.ID, bb.ID = "per-A", "per-B"
	b.onPermissionAsked(a)
	b.onPermissionAsked(bb)

	if err := b.RespondToPermission("per-A", delegator.PermissionDecision{Allow: false}); err != nil {
		t.Fatalf("RespondToPermission returned %v; a 404 on an already-settled alias is not an error", err)
	}
	if n := b.outstanding.Len(); n != 0 {
		t.Errorf("outstanding = %d, want 0 (the settled alias must not stay waiting)", n)
	}
	b.permMu.Lock()
	left := len(b.pendingPerms)
	b.permMu.Unlock()
	if left != 0 {
		t.Errorf("pendingPerms = %d, want 0", left)
	}
}

// TestRespondToPermission_OtherHTTPErrorStillFails is the guard on the fix
// above: only "not found" means settled. Any other failure keeps the member
// tracked and reports the error.
func TestRespondToPermission_OtherHTTPErrorStillFails(t *testing.T) {
	b, _ := newPermTestBackend(t)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(hs.Close)
	b.server = &Server{baseURL: hs.URL, http: hs.Client(), agentID: "perm-test"}

	b.onPermissionAsked(PermissionRequest{ID: "per-X", Permission: PermBash, Patterns: []string{"ls"}, SessionID: "sess-perm"})
	if err := b.RespondToPermission("per-X", delegator.PermissionDecision{Allow: true}); err == nil {
		t.Fatal("RespondToPermission = nil on HTTP 500, want an error")
	}
	if !b.outstanding.Has("per-X") {
		t.Error("per-X dropped from outstanding after a real failure; it must stay tracked")
	}
}

// TestHandleEvent_PermissionRepliedLiveShape proves the live permission.replied
// payload (requestID/reply) reaches onPermissionReplied with the real id, so a
// cascaded or out-of-band settle clears the request and its prompt.
func TestHandleEvent_PermissionRepliedLiveShape(t *testing.T) {
	b, _ := newPermTestBackend(t)
	b.onPermissionAsked(PermissionRequest{ID: "per-C", Permission: PermBash, Patterns: []string{"echo b"}, SessionID: "sess-perm"})

	var reason string
	b.outstanding.AddCancelListener("per-C", func(r string) { reason = r })

	props := json.RawMessage(`{"sessionID":"sess-perm","requestID":"per-C","reply":"reject"}`)
	b.handleEvent(rawEvent{Type: EventPermissionReplied, Properties: props})

	if b.outstanding.Has("per-C") {
		t.Error("per-C still outstanding after a live-shape permission.replied")
	}
	if reason != "replied out-of-band" {
		t.Errorf("cancel listener reason = %q, want it fired with 'replied out-of-band'", reason)
	}
	b.permMu.Lock()
	_, still := b.pendingPerms["per-C"]
	b.permMu.Unlock()
	if still {
		t.Error("per-C still in pendingPerms after permission.replied")
	}
}

// TestHandleEvent_PermissionRepliedLegacyShape keeps the pre-1.17 shape working.
func TestHandleEvent_PermissionRepliedLegacyShape(t *testing.T) {
	b, _ := newPermTestBackend(t)
	b.onPermissionAsked(PermissionRequest{ID: "per-D", Permission: PermBash, Patterns: []string{"echo d"}, SessionID: "sess-perm"})

	props := json.RawMessage(`{"sessionID":"sess-perm","permissionID":"per-D","response":"allow"}`)
	b.handleEvent(rawEvent{Type: EventPermissionReplied, Properties: props})

	if b.outstanding.Has("per-D") {
		t.Error("per-D still outstanding after a legacy-shape permission.replied")
	}
}

// TestRespondToPermission_OwnReplyEchoDoesNotCancelPrompt proves opencode's
// permission.replied echo of foci's OWN reply, arriving before the POST
// returns, resolves the prompt quietly instead of firing the cancel listeners
// (which would rewrite the user's answered prompt as "cancelled").
func TestRespondToPermission_OwnReplyEchoDoesNotCancelPrompt(t *testing.T) {
	b, _ := newPermTestBackend(t)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// opencode publishes the event before it answers the POST.
		b.handleEvent(rawEvent{Type: EventPermissionReplied,
			Properties: json.RawMessage(`{"sessionID":"sess-perm","requestID":"per-E","reply":"once"}`)})
		_, _ = w.Write([]byte(`true`))
	}))
	t.Cleanup(hs.Close)
	b.server = &Server{baseURL: hs.URL, http: hs.Client(), agentID: "perm-test"}

	b.onPermissionAsked(PermissionRequest{ID: "per-E", Permission: PermBash, Patterns: []string{"echo e"}, SessionID: "sess-perm"})
	var cancelled string
	b.outstanding.AddCancelListener("per-E", func(r string) { cancelled = r })

	if err := b.RespondToPermission("per-E", delegator.PermissionDecision{Allow: true}); err != nil {
		t.Fatalf("RespondToPermission: %v", err)
	}
	if cancelled != "" {
		t.Errorf("cancel listener fired (%q) for foci's own reply; the user's answer would show as cancelled", cancelled)
	}
	if b.outstanding.Has("per-E") {
		t.Error("per-E still outstanding")
	}
}
