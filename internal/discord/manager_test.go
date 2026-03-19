package discord

import (
	"testing"
)

func TestBotForSession_NoFacetMatchReturnsNil(t *testing.T) {
	// Verifies that BotForSession returns nil for a regular (non-facet) session key
	// even when a primary bot is registered. BotForSession must only match facet pool
	// entries, never fall back to the primary — that's BotForSessionOrPrimary's job.
	mgr := NewBotManager()
	primary := &Bot{agentID: "clutch"}
	mgr.AddPrimary("clutch", primary)

	facet := &Bot{agentID: "clutch"}
	mgr.AddFacet("clutch", facet)

	// New-format key that parses successfully — before the fix this would
	// have returned the primary bot via the agentID fallback.
	found := mgr.BotForSession("clutch/c12345/1709590000")
	if found != nil {
		t.Errorf("BotForSession should return nil when no facet matches, got %v", found)
	}
}

func TestBotForSessionOrPrimary_NonFacetSessionUsesPrimary(t *testing.T) {
	// Verifies that BotForSessionOrPrimary still falls back to the primary bot
	// for regular session keys that don't match any facet.
	mgr := NewBotManager()
	primary := &Bot{agentID: "clutch"}
	mgr.AddPrimary("clutch", primary)

	facet := &Bot{agentID: "clutch"}
	mgr.AddFacet("clutch", facet)

	bot := mgr.BotForSessionOrPrimary("clutch/c12345/1709590000", "clutch")
	if bot != primary {
		t.Errorf("BotForSessionOrPrimary should fall back to primary for unmatched session key")
	}
}
