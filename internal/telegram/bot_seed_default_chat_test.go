package telegram

import (
	"path/filepath"
	"testing"

	"foci/internal/chatmeta"
	"foci/internal/log"
	"foci/internal/session"
)

// seedTestBot builds a primary bot (non-empty agentID) wired with the given
// allowed users, ready for SetSessionIndex to run the default-chat seed.
func seedTestBot(t *testing.T, allowedUsers []string) *Bot {
	t.Helper()
	lg := log.NewComponentLogger("telegram:seedtest")
	allowed := make(map[string]bool, len(allowedUsers))
	for _, u := range allowedUsers {
		allowed[u] = true
	}
	return &Bot{
		log:          lg,
		agentID:      "seedagent",
		allowedUsers: allowed,
		chatmeta: &chatmeta.Resolver{
			AgentID:      "seedagent",
			PlatformName: platformName,
			Logger:       func() *log.ComponentLogger { return lg },
		},
	}
}

func newSeedIndex(t *testing.T) *session.SessionIndex {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open session index: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func TestSeedDefaultChat_SingleAllowedUser(t *testing.T) {
	b := seedTestBot(t, []string{"5970082313"})
	idx := newSeedIndex(t)

	b.SetSessionIndex(idx) // runs the seed

	if got := idx.DefaultChatForAgent("seedagent", platformName); got != 5970082313 {
		t.Fatalf("default chat = %d; want 5970082313 (seeded from sole allowed user)", got)
	}
}

func TestSeedDefaultChat_ExistingDefaultPreserved(t *testing.T) {
	b := seedTestBot(t, []string{"5970082313"})
	idx := newSeedIndex(t)
	// A real default chat already recorded (e.g. from a prior inbound message).
	if err := idx.SetDefaultChat("seedagent", platformName, 42); err != nil {
		t.Fatalf("pre-set default chat: %v", err)
	}

	b.SetSessionIndex(idx)

	if got := idx.DefaultChatForAgent("seedagent", platformName); got != 42 {
		t.Fatalf("default chat = %d; want 42 (existing default must not be overwritten)", got)
	}
}

func TestSeedDefaultChat_ZeroAllowedUsers(t *testing.T) {
	b := seedTestBot(t, nil)
	idx := newSeedIndex(t)

	b.SetSessionIndex(idx)

	if got := idx.DefaultChatForAgent("seedagent", platformName); got != 0 {
		t.Fatalf("default chat = %d; want 0 (no allowed user to seed from)", got)
	}
}

func TestSeedDefaultChat_MultipleAllowedUsers(t *testing.T) {
	b := seedTestBot(t, []string{"111", "222"})
	idx := newSeedIndex(t)

	b.SetSessionIndex(idx)

	if got := idx.DefaultChatForAgent("seedagent", platformName); got != 0 {
		t.Fatalf("default chat = %d; want 0 (ambiguous owner, must not guess)", got)
	}
}

func TestSeedDefaultChat_NonNumericAllowedUser(t *testing.T) {
	b := seedTestBot(t, []string{"@someuser"})
	idx := newSeedIndex(t)

	b.SetSessionIndex(idx)

	if got := idx.DefaultChatForAgent("seedagent", platformName); got != 0 {
		t.Fatalf("default chat = %d; want 0 (non-numeric entry is not a chat ID)", got)
	}
}

func TestSeedDefaultChat_SecondaryBotSkipped(t *testing.T) {
	b := seedTestBot(t, []string{"5970082313"})
	b.agentID = "" // secondary/facet bot
	b.chatmeta.AgentID = ""
	idx := newSeedIndex(t)

	b.SetSessionIndex(idx)

	if got := idx.DefaultChatForAgent("", platformName); got != 0 {
		t.Fatalf("default chat = %d; want 0 (secondary bot must not seed)", got)
	}
}
