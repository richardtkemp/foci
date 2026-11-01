package platform

// #963: BotManager.StartAgent — an agent hot-added by /agents new starts its
// own bots after StartAll ran, without starting any running bot again.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// countingBot is a PooledBot whose Run counts its starts and blocks until
// ctx ends, then until its exit is released (a shutdown cleanup Wait must
// wait for).
type countingBot struct {
	connectFakeBot
	runs    atomic.Int32
	exited  atomic.Bool
	release chan struct{}
}

func newCountingBot(name string) *countingBot {
	return &countingBot{connectFakeBot: connectFakeBot{name: name}, release: make(chan struct{})}
}

func (b *countingBot) SetSecondary(*Pool[*countingBot]) {}
func (b *countingBot) Run(ctx context.Context) {
	b.runs.Add(1)
	<-ctx.Done()
	<-b.release
	b.exited.Store(true)
}

func TestStartAgent_StartsOnlyTheNewAgentsBots(t *testing.T) {
	m := NewBotManager[*countingBot]("fake")
	oldPrimary, oldFacet, shared := newCountingBot("old"), newCountingBot("old-facet"), newCountingBot("shared")
	m.AddPrimary("old", oldPrimary)
	m.AddFacet("old", oldFacet)
	m.AddSharedFacet(shared)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartAll(ctx)
	waitUntil(t, "boot bots running", func() bool {
		return oldPrimary.runs.Load() == 1 && oldFacet.runs.Load() == 1 && shared.runs.Load() == 1
	})

	// The hot-added agent: a primary (pending connect, as at boot) and a facet.
	newPrimary, newFacet := newCountingBot("new"), newCountingBot("new-facet")
	m.AddPrimary("new", newPrimary)
	m.AddFacet("new", newFacet)
	g := newGate()
	m.ConnectBeforeRun(newPrimary, "new primary", g.connect)

	m.StartAgent(ctx, "new")
	waitUntil(t, "new facet running", func() bool { return newFacet.runs.Load() == 1 })
	waitUntil(t, "new primary connecting", func() bool { return g.calls.Load() == 1 })
	if m.PrimaryBot("new") != nil {
		t.Fatal("pending new primary handed out before its connect succeeded")
	}
	g.release <- nil
	waitUntil(t, "new primary running", func() bool { return newPrimary.runs.Load() == 1 })
	if m.PrimaryBot("new") != newPrimary {
		t.Fatal("new primary not live after its connect")
	}

	// A second StartAgent and a second StartAll start nothing again.
	m.StartAgent(ctx, "new")
	m.StartAll(ctx)
	for _, b := range []*countingBot{oldPrimary, oldFacet, shared, newPrimary, newFacet} {
		if n := b.runs.Load(); n != 1 {
			t.Errorf("bot %s ran %d times, want 1", b.name, n)
		}
	}

	// Shutdown waits for the new agent's bots too.
	cancel()
	waited := make(chan struct{})
	go func() { m.Wait(); close(waited) }()
	select {
	case <-waited:
		t.Fatal("Wait returned while bots were still running their shutdown cleanup")
	case <-time.After(50 * time.Millisecond):
	}
	for _, b := range []*countingBot{oldPrimary, oldFacet, shared, newFacet} {
		close(b.release)
	}
	select {
	case <-waited:
		t.Fatal("Wait returned before the hot-added primary finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(newPrimary.release)
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait never returned")
	}
	if !newPrimary.exited.Load() || !newFacet.exited.Load() {
		t.Fatal("Wait returned before the new agent's bots exited")
	}
}

func TestStartAgent_NothingStartsOnceWaitHasBegun(t *testing.T) {
	m := NewBotManager[*countingBot]("fake")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.StartAll(ctx)
	m.Wait()

	late := newCountingBot("late")
	close(late.release)
	m.AddPrimary("late", late)
	m.StartAgent(ctx, "late")
	m.Wait()
	if n := late.runs.Load(); n != 0 {
		t.Fatalf("a bot started after shutdown began (ran %d times)", n)
	}
}
