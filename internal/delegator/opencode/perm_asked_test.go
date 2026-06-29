package opencode

import (
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/delegator"
)

// findReplyPost returns the most recent POST to /permission/{id}/reply.
func (r *permRecorder) findReplyPost(t *testing.T) permRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.requests) - 1; i >= 0; i-- {
		if strings.Contains(r.requests[i].Path, "/permission/") && strings.HasSuffix(r.requests[i].Path, "/reply") {
			return r.requests[i]
		}
	}
	t.Fatalf("no POST to /permission/{id}/reply found; got %v", r.requests)
	return permRequest{}
}

// TestOnPermissionAsked_SurfacesAndStores proves a permission.asked event
// (opencode 1.2.x) is stored with replyNext=true and a title built from the
// permission kind + patterns, and surfaced via permPromptFn (#arnix-perm).
func TestOnPermissionAsked_SurfacesAndStores(t *testing.T) {
	b, rec := newPermTestBackend(t)

	b.onPermissionAsked(PermissionRequest{
		ID:         "per-bash-1",
		SessionID:  "sess-perm",
		Permission: PermBash,
		Patterns:   []string{"ls -la"},
	})

	b.permMu.Lock()
	pp, ok := b.pendingPerms["per-bash-1"]
	b.permMu.Unlock()
	if !ok || !pp.replyNext || pp.permType != PermBash {
		t.Fatalf("pending = %+v (ok=%v), want {permType=bash, replyNext=true}", pp, ok)
	}

	rec.promptMu.Lock()
	gotID, gotText := rec.lastPrompt.id, rec.lastPrompt.text
	rec.promptMu.Unlock()
	if gotID != "per-bash-1" || gotText != "bash: ls -la" {
		t.Errorf("surfaced id=%q text=%q, want per-bash-1 / 'bash: ls -la'", gotID, gotText)
	}
}

// TestRespondToPermission_Asked_RepliesViaNewEndpoint proves a reply to an
// asked-permission goes to POST /permission/{id}/reply with the mapped reply
// value: allow→once, allow+remember→always, deny→reject.
func TestRespondToPermission_Asked_RepliesViaNewEndpoint(t *testing.T) {
	cases := []struct {
		name      string
		allow     bool
		remember  bool
		wantReply string
	}{
		{"allow once", true, false, "once"},
		{"allow always", true, true, "always"},
		{"deny", false, false, "reject"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, rec := newPermTestBackend(t)
			b.onPermissionAsked(PermissionRequest{ID: "per-x", SessionID: "sess-perm", Permission: PermEdit})

			if err := b.RespondToPermission("per-x", tc.allow, tc.remember); err != nil {
				t.Fatalf("RespondToPermission: %v", err)
			}
			req := rec.findReplyPost(t)
			if req.Path != "/permission/per-x/reply" {
				t.Errorf("path = %q, want /permission/per-x/reply", req.Path)
			}
			var body struct {
				Reply string `json:"reply"`
			}
			_ = json.Unmarshal(req.Body, &body)
			if body.Reply != tc.wantReply {
				t.Errorf("reply = %q, want %q", body.Reply, tc.wantReply)
			}
			// Resolved: pending entry gone.
			b.permMu.Lock()
			_, still := b.pendingPerms["per-x"]
			b.permMu.Unlock()
			if still {
				t.Error("pending permission not cleaned up after reply")
			}
		})
	}
}

// TestFailInFlightTurn_CompletesStuckTurn proves an abnormal session end clears
// turnActive and fires OnTurnComplete, so the Backend doesn't wedge with a
// permanently in-flight turn (the no-respawn / stop-desync bug, #arnix-perm).
func TestFailInFlightTurn_CompletesStuckTurn(t *testing.T) {
	b := &Backend{}
	var completed *delegator.TurnResult
	b.beginTurn(&delegator.TurnEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { completed = r },
	})
	if !b.IsTurnInFlight() {
		t.Fatal("precondition: turn should be in flight after beginTurn")
	}

	b.onSessionError("sess-perm", &MessageError{Name: "UnknownError"})

	if b.IsTurnInFlight() {
		t.Error("turn still in flight after session error — would wedge the Backend")
	}
	if completed == nil || completed.Text == "" {
		t.Errorf("OnTurnComplete not fired with text; got %+v", completed)
	}
}
