package agent

import (
	"sync"
	"testing"
	"time"

	"foci/internal/clock"
	"foci/internal/config"
	"foci/internal/tools"
)

// #3112: the stay-busy inputs the app reads from the agent.

func TestStayBusyWaitThreshold_DefaultAndLive(t *testing.T) {
	if got := (&Agent{}).StayBusyWaitThreshold(); got != time.Hour {
		t.Fatalf("direct agent threshold = %s, want 1h", got)
	}
	snap := &config.ResolvedAgentConfig{}
	snap.Behavior.StayBusyWaitThreshold = "10m"
	var mu sync.Mutex
	a := &Agent{LiveConfigFn: func() *config.ResolvedAgentConfig { mu.Lock(); defer mu.Unlock(); return snap }}
	if got := a.StayBusyWaitThreshold(); got != 10*time.Minute {
		t.Fatalf("live threshold = %s, want 10m", got)
	}
	next := &config.ResolvedAgentConfig{}
	next.Behavior.StayBusyWaitThreshold = "30m"
	mu.Lock()
	snap = next
	mu.Unlock()
	if got := a.StayBusyWaitThreshold(); got != 30*time.Minute {
		t.Fatalf("after a live change: threshold = %s, want 30m", got)
	}
}

func TestAsyncPendingSince_ReadsNotifierAndFiresHook(t *testing.T) {
	clk := clock.NewFake()
	n := tools.NewAsyncNotifier(nil).WithClock(clk)
	a := &Agent{AsyncNotifier: n}
	var mu sync.Mutex
	var calls []string
	a.SetOnWaitInputsChanged(func(sk string) { mu.Lock(); calls = append(calls, sk); mu.Unlock() })

	if _, ok := a.AsyncPendingSince("s"); ok {
		t.Fatal("nothing pending yet")
	}
	n.MarkPending("s")
	first := clk.Now()
	clk.Advance(5 * time.Minute)
	n.MarkPending("s")
	if got, ok := a.AsyncPendingSince("s"); !ok || !got.Equal(first.Add(5*time.Minute)) {
		t.Fatalf("youngest pending = %v %v, want %v", got, ok, first.Add(5*time.Minute))
	}
	n.MarkDone("s")
	n.MarkDone("s")
	if _, ok := a.AsyncPendingSince("s"); ok {
		t.Fatal("still pending after both finished")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 4 {
		t.Fatalf("hook calls = %v, want 4 (two starts, two ends)", calls)
	}
}

func TestUsageHeldInput_MirrorsHeldListsAndFiresHook(t *testing.T) {
	a := &Agent{}
	var calls []string
	a.SetOnWaitInputsChanged(func(sk string) { calls = append(calls, sk) })
	inb := newSessionInbox("s", nil)

	if a.UsageHeldInput("s") {
		t.Fatal("nothing held yet")
	}
	a.parkUsageHeld(inb, Envelope{SessionKey: "s", Text: "hi", Automated: true})
	if !a.UsageHeldInput("s") {
		t.Fatal("a parked user message must read as held")
	}
	inb.usageHeld = nil
	inb.held = []Envelope{{SessionKey: "s", usageHeldID: "x"}} // an inject the hold parked
	a.syncUsageHeldMirror(inb)
	if !a.UsageHeldInput("s") {
		t.Fatal("an inject the usage hold parked must read as held")
	}
	inb.held = []Envelope{{SessionKey: "s"}} // held by pending work, not the usage hold
	a.syncUsageHeldMirror(inb)
	if a.UsageHeldInput("s") {
		t.Fatal("an inject held by background work is not usage-held")
	}
	if len(calls) != 2 {
		t.Fatalf("hook calls = %v, want 2 (held, then released)", calls)
	}
}
