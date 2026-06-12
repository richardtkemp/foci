package discord

import (
	"context"
	"testing"
)

// TestConnectionManagerAdapterNilSafety verifies all adapter accessors return
// true nil interfaces (not typed-nil wrappers) when no bot matches.
func TestConnectionManagerAdapterNilSafety(t *testing.T) {
	a := &ConnectionManagerAdapter{BotManager: NewBotManager()}

	if a.Primary("ghost") != nil {
		t.Error("Primary should return nil interface")
	}
	if a.AllForAgent("ghost") != nil {
		t.Error("AllForAgent should return nil")
	}
	if a.ForSession("nope") != nil {
		t.Error("ForSession should return nil interface")
	}
	if a.ForSessionOrPrimary("nope", "ghost") != nil {
		t.Error("ForSessionOrPrimary should return nil interface")
	}
	if conn, ok := a.AcquireFacet("ghost"); ok || conn != nil {
		t.Error("AcquireFacet should fail with nil interface")
	}
	if a.HasFacet("ghost") {
		t.Error("HasFacet should be false")
	}

	// StartAll/Wait on an empty manager delegate and return immediately.
	ctx, cancel := context.WithCancel(context.Background())
	a.StartAll(ctx)
	cancel()
	a.Wait()
}

// TestConnectionManagerAdapterWrapping verifies the adapter surfaces registered
// bots as platform.Connection values.
func TestConnectionManagerAdapterWrapping(t *testing.T) {
	a := &ConnectionManagerAdapter{BotManager: NewBotManager()}
	primary := &Bot{agentID: "a", sessionKey: "a/c1/123"}
	a.AddPrimary("a", primary)
	facet := &Bot{}
	a.AddFacet("a", facet)

	if got := a.Primary("a"); got != primary {
		t.Error("Primary should return the registered bot")
	}
	all := a.AllForAgent("a")
	if len(all) != 1 || all[0] != primary {
		t.Errorf("AllForAgent: got %v", all)
	}
	if got := a.ForSession("a/c1/123"); got != primary {
		t.Error("ForSession should match the primary's session key")
	}
	if got := a.ForSessionOrPrimary("unmatched", "a"); got != primary {
		t.Error("ForSessionOrPrimary should fall back to primary")
	}
	conn, ok := a.AcquireFacet("a")
	if !ok || conn != facet {
		t.Error("AcquireFacet should return the facet bot")
	}
	if !a.HasFacet("a") {
		t.Error("HasFacet should be true")
	}
}
