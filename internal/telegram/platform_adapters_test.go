package telegram

import (
	"testing"

	"foci/internal/command"
	"foci/internal/platform"
)

// adapterFixture builds a ConnectionManagerAdapter over a BotManager with one
// primary bot ("scout") and one idle facet bot in scout's pool.
func adapterFixture(t *testing.T) (*platform.ConnectionManagerAdapter[*Bot], *Bot, *Bot) {
	t.Helper()
	mgr := NewBotManager()
	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("scout", primary)
	facet, _ := testBot(nil, command.NewRegistry())
	facet.SetSessionKeyDirect("") // idle, acquirable
	mgr.AddFacet("scout", facet)
	return platform.NewConnectionManagerAdapter[*Bot](mgr), primary, facet
}

func TestAdapter_Primary(t *testing.T) {
	// Proves Primary returns the agent's primary bot as a platform.Connection,
	// and a true nil interface (not a typed-nil *Bot) for unknown agents.
	a, primary, _ := adapterFixture(t)

	if got := a.Primary("scout"); got != platform.Connection(primary) {
		t.Errorf("Primary(scout) = %v, want the primary bot", got)
	}
	if got := a.Primary("unknown"); got != nil {
		t.Errorf("Primary(unknown) = %v, want untyped nil", got)
	}
}

func TestAdapter_AllForAgent(t *testing.T) {
	// Proves AllForAgent returns a one-element slice holding the primary bot,
	// and nil for unknown agents.
	a, primary, _ := adapterFixture(t)

	conns := a.AllForAgent("scout")
	if len(conns) != 1 || conns[0] != platform.Connection(primary) {
		t.Errorf("AllForAgent(scout) = %v, want [primary]", conns)
	}
	if got := a.AllForAgent("unknown"); got != nil {
		t.Errorf("AllForAgent(unknown) = %v, want nil", got)
	}
}

func TestAdapter_ForSession(t *testing.T) {
	// Proves ForSession resolves the bot holding a session key and returns a
	// true nil interface when no bot owns the key.
	a, primary, _ := adapterFixture(t)

	if got := a.ForSession("agent:test:main"); got != platform.Connection(primary) {
		t.Errorf("ForSession = %v, want primary", got)
	}
	if got := a.ForSession("agent:nobody:main"); got != nil {
		t.Errorf("ForSession(unknown key) = %v, want untyped nil", got)
	}
}

func TestAdapter_ForSessionOrPrimary(t *testing.T) {
	// Proves ForSessionOrPrimary falls back to the agent's primary bot for an
	// unowned session key, and returns untyped nil when neither exists.
	a, primary, _ := adapterFixture(t)

	if got := a.ForSessionOrPrimary("agent:nobody:main", "scout"); got != platform.Connection(primary) {
		t.Errorf("fallback = %v, want primary", got)
	}
	if got := a.ForSessionOrPrimary("agent:nobody:main", "unknown"); got != nil {
		t.Errorf("no match = %v, want untyped nil", got)
	}
}

func TestAdapter_AcquireFacetAndHasFacet(t *testing.T) {
	// Proves AcquireFacet hands out an idle facet bot exactly once (second
	// acquire fails with untyped nil) and HasFacet reports pool availability.
	a, _, facet := adapterFixture(t)

	if !a.HasFacet("scout") {
		t.Error("HasFacet(scout) = false, want true")
	}
	if a.HasFacet("unknown") {
		t.Error("HasFacet(unknown) = true, want false")
	}

	conn, ok := a.AcquireFacet("scout")
	if !ok || conn != platform.Connection(facet) {
		t.Fatalf("AcquireFacet = %v/%v, want facet/true", conn, ok)
	}
	// Mark busy (Acquire doesn't set the key itself).
	facet.SetSessionKeyDirect("agent:scout:branch:x")

	conn, ok = a.AcquireFacet("scout")
	if ok || conn != nil {
		t.Errorf("second AcquireFacet = %v/%v, want untyped nil/false", conn, ok)
	}
}
