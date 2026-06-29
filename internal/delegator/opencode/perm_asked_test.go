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

// TestPermissionAsked_DedupesSameTarget proves that two distinct permission
// objects for the SAME target (opencode raises one per tool call) surface only
// ONE prompt, and that answering it fans the decision out to BOTH opencode
// permissions — each blocks its own tool call, so both must be replied to.
func TestPermissionAsked_DedupesSameTarget(t *testing.T) {
	b, rec := newPermTestBackend(t)

	var prompts int
	rec.promptMu.Lock()
	b.permPromptFn = func(id, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
		rec.promptMu.Lock()
		prompts++
		rec.promptMu.Unlock()
	}
	rec.promptMu.Unlock()

	// Two distinct IDs, identical target (kind + patterns) — the arnix runtime.
	target := PermissionRequest{Permission: PermExternalDirectory, Patterns: []string{"/home/rich/git/foci/*"}, SessionID: "sess-perm"}
	asked1 := target
	asked1.ID = "per-A"
	asked2 := target
	asked2.ID = "per-B"
	b.onPermissionAsked(asked1)
	b.onPermissionAsked(asked2)

	rec.promptMu.Lock()
	gotPrompts := prompts
	rec.promptMu.Unlock()
	if gotPrompts != 1 {
		t.Fatalf("surfaced %d prompts, want 1 (dedup)", gotPrompts)
	}

	// Both must still be registered as outstanding (opencode blocks each).
	if n := b.outstanding.Len(); n != 2 {
		t.Fatalf("outstanding = %d, want 2 (primary + alias both block)", n)
	}

	// The user answers the primary (per-A). Both opencode permissions must get a
	// reply POST, and both must be cleared.
	if err := b.RespondToPermission("per-A", true, true); err != nil {
		t.Fatalf("RespondToPermission: %v", err)
	}

	rec.mu.Lock()
	var replied []string
	for _, req := range rec.requests {
		if strings.HasSuffix(req.Path, "/reply") {
			replied = append(replied, req.Path)
		}
	}
	rec.mu.Unlock()
	if len(replied) != 2 {
		t.Fatalf("got %d reply POSTs %v, want 2 (one per group member)", len(replied), replied)
	}
	wantA, wantB := false, false
	for _, p := range replied {
		switch p {
		case "/permission/per-A/reply":
			wantA = true
		case "/permission/per-B/reply":
			wantB = true
		}
	}
	if !wantA || !wantB {
		t.Errorf("reply paths = %v, want both per-A and per-B", replied)
	}

	if n := b.outstanding.Len(); n != 0 {
		t.Errorf("outstanding = %d after answer, want 0", n)
	}
	b.permMu.Lock()
	left := len(b.pendingPerms)
	b.permMu.Unlock()
	if left != 0 {
		t.Errorf("pendingPerms = %d after answer, want 0", left)
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
