package telegram

import (
	"testing"

	"clod/command"
)

func TestBotManagerPrimary(t *testing.T) {
	mgr := NewBotManager()

	bot1, _ := testBot(nil, command.NewRegistry())
	bot2, _ := testBot(nil, command.NewRegistry())

	mgr.AddPrimary("clutch", bot1)
	mgr.AddPrimary("scout", bot2)

	if got := mgr.PrimaryBot("clutch"); got != bot1 {
		t.Errorf("PrimaryBot(clutch) = %v, want bot1", got)
	}
	if got := mgr.PrimaryBot("scout"); got != bot2 {
		t.Errorf("PrimaryBot(scout) = %v, want bot2", got)
	}
	if got := mgr.PrimaryBot("unknown"); got != nil {
		t.Errorf("PrimaryBot(unknown) = %v, want nil", got)
	}

	ids := mgr.AgentIDs()
	if len(ids) != 2 {
		t.Fatalf("AgentIDs len = %d, want 2", len(ids))
	}
}

func TestBotManagerMultiball(t *testing.T) {
	mgr := NewBotManager()

	primary, _ := testBot(nil, command.NewRegistry())
	mgr.AddPrimary("clutch", primary)

	// Add two multiball bots for clutch
	mb1, _ := testBot(nil, command.NewRegistry())
	mb2, _ := testBot(nil, command.NewRegistry())
	mgr.AddMultiball("clutch", mb1)
	mgr.AddMultiball("clutch", mb2)

	pool := mgr.Pool("clutch")
	if pool == nil {
		t.Fatal("Pool(clutch) = nil, want pool")
	}
	if pool.Size() != 2 {
		t.Errorf("pool size = %d, want 2", pool.Size())
	}

	// mb1 and mb2 should be marked as secondary
	if !mb1.isSecondary {
		t.Error("mb1 not marked secondary")
	}
	if !mb2.isSecondary {
		t.Error("mb2 not marked secondary")
	}

	// Scout has no multiball
	if got := mgr.Pool("scout"); got != nil {
		t.Errorf("Pool(scout) = %v, want nil", got)
	}
}
