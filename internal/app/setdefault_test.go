package app

import (
	"testing"

	"foci/internal/fap"
	"foci/internal/platform"
)

// TestHandleConversationSetDefault_PersistsAndClears proves the set-default
// handler stores the agent's app-platform default chat (keyed by the stable
// chatID) and that clearing removes it. Mirrors the rename handler test.
func TestHandleConversationSetDefault_PersistsAndClears(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	b := withIdent(&convBinding{convID: "c1", chatID: 42}, "clutch", "")
	h.convs["c1"] = b

	// Set.
	h.handleConversationSetDefault(fap.ConversationSetDefault{ConversationID: "c1", IsDefault: true})
	if got := idx.DefaultChatForAgent("clutch", "app"); got != 42 {
		t.Fatalf("default after set = %d, want 42", got)
	}

	// Clear.
	h.handleConversationSetDefault(fap.ConversationSetDefault{ConversationID: "c1", IsDefault: false})
	if got := idx.DefaultChatForAgent("clutch", "app"); got != 0 {
		t.Fatalf("default after clear = %d, want 0", got)
	}

	// Unknown conversation: no panic, no write.
	h.handleConversationSetDefault(fap.ConversationSetDefault{ConversationID: "ghost", IsDefault: true})
	if got := idx.DefaultChatForAgent("clutch", "app"); got != 0 {
		t.Fatalf("default after ghost set = %d, want 0", got)
	}
}

// TestHandleConversationSetDefault_MovesDefault proves setting a new default
// chat clears the previous one (one default per agent+platform).
func TestHandleConversationSetDefault_MovesDefault(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = withIdent(&convBinding{convID: "c1", chatID: 42}, "clutch", "")
	h.convs["c2"] = withIdent(&convBinding{convID: "c2", chatID: 99}, "clutch", "")

	h.handleConversationSetDefault(fap.ConversationSetDefault{ConversationID: "c1", IsDefault: true})
	h.handleConversationSetDefault(fap.ConversationSetDefault{ConversationID: "c2", IsDefault: true})

	if got := idx.DefaultChatForAgent("clutch", "app"); got != 99 {
		t.Fatalf("default after move = %d, want 99 (c2 replaces c1)", got)
	}
}

// TestAgentRoster_MarksDefaultConversation proves the roster surfaces IsDefault
// for the chat that is the agent's app-platform default, and only that chat.
func TestAgentRoster_MarksDefaultConversation(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	registerBareAgent(h, "ag")
	h.convs["c1"] = withIdent(&convBinding{convID: "c1", chatID: 42}, "ag", "")
	h.convs["c2"] = withIdent(&convBinding{convID: "c2", chatID: 99}, "ag", "")
	if err := idx.SetDefaultChat("ag", "app", 99); err != nil {
		t.Fatal(err)
	}

	roster := h.agentRoster()
	if len(roster) != 1 {
		t.Fatalf("roster = %d agents, want 1", len(roster))
	}
	var defaults, total int
	for _, ci := range roster[0].Conversations {
		total++
		if ci.IsDefault {
			defaults++
			if ci.ID != "c2" {
				t.Errorf("IsDefault set on %q, want c2 (chatID 99)", ci.ID)
			}
		}
	}
	if total != 2 || defaults != 1 {
		t.Fatalf("roster convs total=%d defaults=%d, want 2/1", total, defaults)
	}
}
