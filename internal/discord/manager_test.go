package discord

import (
	"testing"
)

func TestBotForSession_NoBotMatchReturnsNil(t *testing.T) {
	// Verifies that BotForSession returns nil when no bot's SessionKey matches.
	mgr := NewBotManager()
	primary := &Bot{agentID: "clutch"}
	mgr.AddPrimary("clutch", primary)

	facet := &Bot{agentID: "clutch"}
	mgr.AddFacet("clutch", facet)

	found := mgr.BotForSession("clutch/c12345")
	if found != nil {
		t.Errorf("BotForSession should return nil when no bot has this session key, got %v", found)
	}
}

func TestBotForSession_MatchesPrimaryBot(t *testing.T) {
	// Verifies that BotForSession returns the primary bot when its SessionKey matches.
	mgr := NewBotManager()
	primary := &Bot{agentID: "clutch", sessionKey: "clutch/c12345"}
	mgr.AddPrimary("clutch", primary)

	facet := &Bot{agentID: "clutch"}
	mgr.AddFacet("clutch", facet)

	found := mgr.BotForSession("clutch/c12345")
	if found != primary {
		t.Errorf("BotForSession should return primary bot when its SessionKey matches")
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

	bot := mgr.BotForSessionOrPrimary("clutch/c12345", "clutch")
	if bot != primary {
		t.Errorf("BotForSessionOrPrimary should fall back to primary for unmatched session key")
	}
}
