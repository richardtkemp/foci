package slack

// conn_test.go: the connection capability facts — approvals are refused on
// both the bound view and the unbound instance (review note 3), and the
// SessionKey contract.

import (
	"testing"

	"foci/internal/log"
	"foci/internal/platform"
)

func init() { log.SetWarnHook(nil) }

// TestConnRefusesApprovals proves RefusesApprovals is true on the bound view
// AND on the unbound per-agent instance (the one ForSessionOrPrimary hands
// agent-wide and session-less callers): both must fail approval prompts
// closed (#2459 A1).
func TestConnRefusesApprovals(t *testing.T) {
	h := newHarness(t, "helen", nil)
	unbound := h.mgr.PrimaryBot("helen")
	if unbound == nil {
		t.Fatal("no unbound connection")
	}
	if !unbound.RefusesApprovals() {
		t.Error("unbound connection does not refuse approvals")
	}
	bound := h.boundConn("helen", Target{Kind: TargetDM, Channel: "D1", User: "U1"})
	if !bound.RefusesApprovals() {
		t.Error("bound view does not refuse approvals")
	}
	// And through the helper, as the gateway reads it.
	if !platform.RefusesApprovals(unbound) || !platform.RefusesApprovals(bound) {
		t.Error("platform.RefusesApprovals disagrees on a slack connection")
	}
}

// TestConnSessionKeyContract proves the view contract: the bound view
// reports its own session; the unbound instance reports none.
func TestConnSessionKeyContract(t *testing.T) {
	h := newHarness(t, "helen", nil)
	target := Target{Kind: TargetDM, Channel: "D1", User: "U1"}
	bound := h.boundConn("helen", target)
	if got := bound.SessionKey(); got != sessionKeyFor("helen", target) {
		t.Errorf("bound SessionKey = %q, want its own session", got)
	}
	if got := bound.DefaultSessionKey(); got != sessionKeyFor("helen", target) {
		t.Errorf("bound DefaultSessionKey = %q, want its own session", got)
	}
	if got := h.mgr.PrimaryBot("helen").SessionKey(); got != "" {
		t.Errorf("unbound SessionKey = %q, want \"\"", got)
	}
	if got := h.mgr.PrimaryBot("helen").DefaultSessionKey(); got != "" {
		t.Errorf("unbound DefaultSessionKey = %q, want \"\" (no default chat exists)", got)
	}
}

// TestConnDropsAgentWideSends proves the broadcast capability split: the
// UNBOUND instance (what a fan-out would address) drops agent-wide sends —
// so route.Broadcast skips it — while a bound view has its session and does
// not. The helper is nil-safe, as the fan-out reads it.
func TestConnDropsAgentWideSends(t *testing.T) {
	h := newHarness(t, "helen", nil)
	unbound := h.mgr.PrimaryBot("helen")
	if unbound == nil {
		t.Fatal("no unbound connection")
	}
	if !unbound.DropsAgentWideSends() || !platform.DropsAgentWideSends(unbound) {
		t.Error("unbound connection does not declare the agent-wide drop; a fan-out must skip it")
	}
	bound := h.boundConn("helen", Target{Kind: TargetDM, Channel: "D1", User: "U1"})
	if bound.DropsAgentWideSends() || platform.DropsAgentWideSends(bound) {
		t.Error("bound view declares the agent-wide drop; it addresses its own session")
	}
	if platform.DropsAgentWideSends(nil) {
		t.Error("DropsAgentWideSends(nil) = true; the helper must be nil-safe")
	}
}

// TestConnConfinesReplies proves the reply-confinement capability on both
// views: a Slack session's reply text never leaves its own conversation,
// so broadcastResponse delivers it there only (#2459 req 5). True on the
// unbound instance too — confinement is a property of the platform, not of
// the view.
func TestConnConfinesReplies(t *testing.T) {
	h := newHarness(t, "helen", nil)
	unbound := h.mgr.PrimaryBot("helen")
	if unbound == nil {
		t.Fatal("no unbound connection")
	}
	if !unbound.ConfinesReplies() || !platform.ConfinesReplies(unbound) {
		t.Error("unbound connection does not confine replies; a broadcast turn must not be fanned out")
	}
	bound := h.boundConn("helen", Target{Kind: TargetDM, Channel: "D1", User: "U1"})
	if !bound.ConfinesReplies() || !platform.ConfinesReplies(bound) {
		t.Error("bound view does not confine replies; the session's text reaches its own conversation only")
	}
}
