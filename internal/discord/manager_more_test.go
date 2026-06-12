package discord

import (
	"context"
	"sort"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// TestManagerPoolsAndSharedFacet verifies per-agent and shared facet pools are
// created on demand and the registered bots are marked secondary.
func TestManagerPoolsAndSharedFacet(t *testing.T) {
	mgr := NewBotManager()
	if mgr.Pool("a") != nil || mgr.SharedPool() != nil {
		t.Fatal("expected no pools on fresh manager")
	}

	facet := &Bot{}
	mgr.AddFacet("a", facet)
	pool := mgr.Pool("a")
	if pool == nil || pool.Size() != 1 {
		t.Fatal("expected per-agent pool with 1 bot")
	}
	if !facet.isSecondary || facet.pool != pool {
		t.Error("facet bot should be secondary and wired to its pool")
	}

	shared := &Bot{}
	mgr.AddSharedFacet(shared)
	sp := mgr.SharedPool()
	if sp == nil || sp.Size() != 1 {
		t.Fatal("expected shared pool with 1 bot")
	}
	if !shared.isSecondary || shared.pool != sp {
		t.Error("shared facet bot should be secondary and wired to the shared pool")
	}
}

// TestManagerAcquireFacet verifies acquisition priority: the per-agent pool is
// tried first, then the shared pool, and failure when both are exhausted.
func TestManagerAcquireFacet(t *testing.T) {
	mgr := NewBotManager()
	agentBot := &Bot{}
	sharedBot := &Bot{}
	mgr.AddFacet("a", agentBot)
	mgr.AddSharedFacet(sharedBot)

	got, ok := mgr.AcquireFacet("a")
	if !ok || got != agentBot {
		t.Fatal("expected per-agent bot first")
	}
	got.SetSessionKey("busy-1")

	got, ok = mgr.AcquireFacet("a")
	if !ok || got != sharedBot {
		t.Fatal("expected shared-pool fallback")
	}
	got.SetSessionKey("busy-2")

	if _, ok := mgr.AcquireFacet("a"); ok {
		t.Error("expected failure when all pools exhausted")
	}

	// Agent with no per-agent pool goes straight to shared.
	sharedBot.SetSessionKey("")
	got, ok = mgr.AcquireFacet("other")
	if !ok || got != sharedBot {
		t.Error("expected shared bot for agent without its own pool")
	}
}

// TestManagerHasFacet verifies facet availability checks across per-agent and
// shared pools.
func TestManagerHasFacet(t *testing.T) {
	mgr := NewBotManager()
	if mgr.HasFacet("a") {
		t.Error("fresh manager should have no facets")
	}

	mgr.AddFacet("a", &Bot{})
	if !mgr.HasFacet("a") {
		t.Error("expected per-agent facet")
	}
	if mgr.HasFacet("b") {
		t.Error("agent b has no facets and no shared pool exists")
	}

	mgr.AddSharedFacet(&Bot{})
	if !mgr.HasFacet("b") {
		t.Error("expected shared facet to count for any agent")
	}
}

// TestManagerAgentIDs verifies AgentIDs lists every agent with a primary bot.
func TestManagerAgentIDs(t *testing.T) {
	mgr := NewBotManager()
	mgr.AddPrimary("a", &Bot{})
	mgr.AddPrimary("b", &Bot{})

	ids := mgr.AgentIDs()
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Errorf("unexpected agent IDs %v", ids)
	}
}

// TestManagerStartAllWait verifies StartAll launches each bot's Run loop and
// Wait returns once the context is cancelled — using a real (unopened)
// discordgo session so no network is touched.
func TestManagerStartAllWait(t *testing.T) {
	dg, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{session: dg, api: dg}
	bot.mq = newTestBotMQ()

	mgr := NewBotManager()
	mgr.AddPrimary("a", bot)

	ctx, cancel := context.WithCancel(context.Background())
	mgr.StartAll(ctx)
	cancel()
	mgr.Wait() // must return; hangs (test timeout) if shutdown is broken
}
