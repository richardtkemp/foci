package opencode

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"foci/internal/delegator"
)

// startAttended is startUnattended (skip_permissions_test.go) with prompting
// ENABLED: an ordinary attended session. System-turn unattended answering is
// tested by beginning a turn whose TurnEvents carries Unattended (exactly
// what buildTurnEvents in the agent layer installs for a non-user trigger).
func startAttended(t *testing.T) (*Backend, func() []permRequest, func() []string) {
	t.Helper()
	return startUnattended(t, false, false)
}

// beginSystemTurn arms the turn-state a system-triggered turn would install:
// beginTurn with TurnEvents{Trigger, Unattended: true}.
func beginSystemTurn(b *Backend, trigger string) {
	b.beginTurn(&delegator.TurnEvents{Trigger: trigger, Unattended: true})
}

// TestUnattendedTurn_AllowsAskedPermissionWithoutPrompt: a permission.asked
// raised while a system-triggered turn (reflection) is in flight on an
// ATTENDED session is answered allow-once by the backend — no prompt, nothing
// pending, nothing outstanding. Regression for the 2026-10-27 incident
// (external_directory ask blocking 89 minutes).
func TestUnattendedTurn_AllowsAskedPermissionWithoutPrompt(t *testing.T) {
	b, reqs, prompted := startAttended(t)
	beginSystemTurn(b, "reflection")
	b.onPermissionAsked(PermissionRequest{
		ID: "per-u1", SessionID: "sess-batch", Permission: "external_directory", Patterns: []string{"../outside/**"},
	})
	assertUnattendedReply(t, b, reqs(), prompted(), "/permission/per-u1/reply", "reply", "once")
}

// TestUnattendedTurn_AllowsLegacyPermissionWithoutPrompt covers the older
// permission.updated model during a system turn.
func TestUnattendedTurn_AllowsLegacyPermissionWithoutPrompt(t *testing.T) {
	b, reqs, prompted := startAttended(t)
	beginSystemTurn(b, "keepalive")
	b.onPermissionUpdated(Permission{ID: "per-u2", Type: PermBash, Title: "bash: rm -rf build"})
	assertUnattendedReply(t, b, reqs(), prompted(), "/session/sess-batch/permissions/per-u2", "response", "allow")
}

// TestUnattendedTurn_DeclinesQuestionWithoutPrompt: a question during a
// system turn has nobody to answer it — declined, never surfaced.
func TestUnattendedTurn_DeclinesQuestionWithoutPrompt(t *testing.T) {
	b, reqs, prompted := startAttended(t)
	beginSystemTurn(b, "reflection")
	b.onPermissionUpdated(Permission{
		ID: "per-uq", Type: PermQuestion, Title: "pick",
		Metadata: json.RawMessage(`{"text":"which?","options":[{"label":"a"}]}`),
	})
	assertUnattendedReply(t, b, reqs(), prompted(), "/session/sess-batch/permissions/per-uq", "response", "deny")
}

// TestUnattendedTurn_UserTurnStillPrompts is the regression guard: the SAME
// attended session, a turn that is NOT system-triggered — the ask surfaces
// via permPromptFn exactly as before, and no reply is sent behind the user's
// back.
func TestUnattendedTurn_UserTurnStillPrompts(t *testing.T) {
	b, reqs, prompted := startAttended(t)
	b.beginTurn(&delegator.TurnEvents{Trigger: "telegram"})
	b.onPermissionAsked(PermissionRequest{
		ID: "per-u3", SessionID: "sess-batch", Permission: PermBash, Patterns: []string{"rm -rf build"},
	})
	if got := prompted(); len(got) != 1 || got[0] != "per-u3" {
		t.Errorf("prompted = %v, want [per-u3]", got)
	}
	for _, r := range reqs() {
		if strings.Contains(r.Path, "per-u3") {
			t.Errorf("unexpected reply %s %s for a user turn", r.Path, r.Body)
		}
	}
	b.permMu.Lock()
	if _, ok := b.pendingPerms["per-u3"]; !ok {
		t.Error("user-turn ask not stored pending; the keyboard would have nothing to answer")
	}
	b.permMu.Unlock()
}

// TestUnattendedTurn_AskAfterTurnEndStillPrompts: the unattended flag is
// turn-scoped — once the system turn's state is cleared, an ask on the same
// attended session surfaces to the user again.
func TestUnattendedTurn_AskAfterTurnEndStillPrompts(t *testing.T) {
	b, _, prompted := startAttended(t)
	beginSystemTurn(b, "reflection")
	b.cancelTurn() // reverses beginTurn, as a failed send would

	b.onPermissionAsked(PermissionRequest{
		ID: "per-u4", SessionID: "sess-batch", Permission: PermBash, Patterns: []string{"ls"},
	})
	if got := prompted(); len(got) != 1 || got[0] != "per-u4" {
		t.Errorf("prompted = %v after system turn ended, want [per-u4]", got)
	}
}

// TestUnattendedTurn_ConcurrentAskSerialization is a light race check: asks
// surfacing on the dispatcher goroutine read the turn flag under turnMu
// while turns begin/end on the inject path. Run with -race.
func TestUnattendedTurn_ConcurrentAskSerialization(t *testing.T) {
	b, _, _ := startAttended(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); beginSystemTurn(b, "reflection") }()
		go func() {
			defer wg.Done()
			b.onPermissionAsked(PermissionRequest{ID: "per-x", SessionID: "sess-batch", Permission: PermRead, Patterns: []string{"a.go"}})
		}()
	}
	wg.Wait()
}
