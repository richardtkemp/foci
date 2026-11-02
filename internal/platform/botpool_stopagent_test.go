package platform

// #2787: BotManager.StopAgent — the live stop of one agent ends its own bots
// (booted or hot-added), waits for their cleanup, and forgets them; every
// other agent's bots and the shared pool run on.

import (
	"context"
	"testing"
	"time"
)

func TestStopAgent_StopsOnlyThatAgentsBotsAndWaits(t *testing.T) {
	m := NewBotManager[*countingBot]("fake")
	bob, bobFacet := newCountingBot("bob"), newCountingBot("bob-facet")
	carol, shared := newCountingBot("carol"), newCountingBot("shared")
	m.AddPrimary("bob", bob)
	m.AddFacet("bob", bobFacet)
	m.AddPrimary("carol", carol)
	m.AddSharedFacet(shared)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartAll(ctx)
	waitUntil(t, "boot bots running", func() bool {
		return bob.runs.Load() == 1 && bobFacet.runs.Load() == 1 && carol.runs.Load() == 1 && shared.runs.Load() == 1
	})

	stopped := make(chan struct{})
	go func() { m.StopAgent("bob"); close(stopped) }()
	// StopAgent waits for bob's bots' cleanup.
	select {
	case <-stopped:
		t.Fatal("StopAgent returned before bob's bots finished their cleanup")
	case <-time.After(50 * time.Millisecond):
	}
	close(bob.release)
	close(bobFacet.release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("StopAgent did not return after bob's bots exited")
	}
	if !bob.exited.Load() || !bobFacet.exited.Load() {
		t.Error("bob's bots did not exit")
	}
	if carol.exited.Load() || shared.exited.Load() {
		t.Error("a bot of another agent, or of the shared pool, exited")
	}
	if m.PrimaryBot("bob") != nil || m.Pool("bob") != nil { // (HasFacet falls back to the shared pool)
		t.Error("bob's bots are still handed out after StopAgent")
	}
	if m.PrimaryBot("carol") != carol {
		t.Error("carol's primary is gone")
	}

	// A renamed agent's new bot (the same token in real life) starts clean.
	rob := newCountingBot("rob")
	m.AddPrimary("rob", rob)
	m.StartAgent(ctx, "rob")
	waitUntil(t, "rob running", func() bool { return rob.runs.Load() == 1 })

	// Shutdown still waits for every bot that is left.
	close(carol.release)
	close(shared.release)
	close(rob.release)
	cancel()
	waited := make(chan struct{})
	go func() { m.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return")
	}
}

// A hot-added agent's bots stop the same way as booted ones.
func TestStopAgent_HotAddedAgent(t *testing.T) {
	m := NewBotManager[*countingBot]("fake")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartAll(ctx)
	bob := newCountingBot("bob")
	m.AddPrimary("bob", bob)
	m.StartAgent(ctx, "bob")
	waitUntil(t, "bob running", func() bool { return bob.runs.Load() == 1 })
	close(bob.release)
	m.StopAgent("bob")
	if !bob.exited.Load() {
		t.Error("StopAgent returned before the hot-added bot exited")
	}
	if ids := m.AgentIDs(); len(ids) != 0 {
		t.Errorf("AgentIDs after stop = %v", ids)
	}
}
