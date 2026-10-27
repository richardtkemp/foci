package opencode

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"foci/internal/delegator"
	"foci/internal/delegator/autoapprove"
	"foci/internal/log"
)

// startAttended is startUnattended (skip_permissions_test.go) with prompting
// ENABLED: an ordinary attended session. System-turn unattended answering is
// tested by beginning a turn whose TurnEvents carries Unattended (exactly
// what buildTurnEvents in the agent layer installs for an unattended trigger).
func startAttended(t *testing.T) (*Backend, func() []permRequest, func() []string) {
	t.Helper()
	return startUnattended(t, false, false)
}

// syncBuffer is a goroutine-safe bytes.Buffer for capturing log output.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// beginSystemTurn arms the turn-state an unattended turn would install:
// beginTurn with TurnEvents{Trigger, Unattended: true}.
func beginSystemTurn(b *Backend, trigger string) {
	b.beginTurn(&delegator.TurnEvents{Trigger: trigger, Unattended: true})
}

// TestUnattendedTurn_DeniesUnruledAskWithWarning is the 2026-10-27 incident
// under the ruling: a permission.asked raised while an unattended turn
// (reflection) is in flight on an ATTENDED session is decided by foci's rules.
// external_directory has no rule mapping, so it is rejected at once — no
// prompt, nothing pending, nothing outstanding — with a WARN naming the
// session, the trigger and what was refused.
func TestUnattendedTurn_DeniesUnruledAskWithWarning(t *testing.T) {
	var logBuf syncBuffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(nil) })

	b, reqs, prompted := startAttended(t)
	b.startOpts.SessionKey = "arnix/c1"
	b.autoApproveRules = autoapprove.Compile(autoapprove.CommonReadonlyRules)
	beginSystemTurn(b, "reflection")
	b.onPermissionAsked(PermissionRequest{
		ID: "per-u1", SessionID: "sess-batch", Permission: "external_directory", Patterns: []string{"../outside/**"},
	})
	assertUnattendedReply(t, b, reqs(), prompted(), "/permission/per-u1/reply", "reply", "reject")

	want := "unattended: session arnix/c1 (trigger=reflection) tried to use external_directory: ../outside/**; no rule permits it, so it was denied"
	if got := logBuf.String(); !strings.Contains(got, want) || !strings.Contains(got, "WARN") {
		t.Errorf("log missing WARN %q; got:\n%s", want, got)
	}
}

// TestUnattendedTurn_AllowsRuledAsk: the same unattended turn, an ask a rule
// permits — allowed once, still without a prompt.
func TestUnattendedTurn_AllowsRuledAsk(t *testing.T) {
	b, reqs, prompted := startAttended(t)
	b.autoApproveRules = autoapprove.Compile([]string{"Bash:ls"})
	beginSystemTurn(b, "keepalive")
	b.onPermissionAsked(PermissionRequest{
		ID: "per-u2", SessionID: "sess-batch", Permission: PermBash, Patterns: []string{"ls"},
	})
	assertUnattendedReply(t, b, reqs(), prompted(), "/permission/per-u2/reply", "reply", "once")
}

// TestUnattendedTurn_SubstitutionAskDenied: an rm rule exists but the engine
// refuses to match a live command-substitution argument — denied, not
// prompted.
func TestUnattendedTurn_SubstitutionAskDenied(t *testing.T) {
	b, reqs, prompted := startAttended(t)
	b.autoApproveRules = autoapprove.Compile([]string{"Bash:rm *"})
	beginSystemTurn(b, "reflection")
	b.onPermissionAsked(PermissionRequest{
		ID: "per-u5", SessionID: "sess-batch", Permission: PermBash, Patterns: []string{`rm -rf "$(pwd)"`},
	})
	assertUnattendedReply(t, b, reqs(), prompted(), "/permission/per-u5/reply", "reply", "reject")
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
