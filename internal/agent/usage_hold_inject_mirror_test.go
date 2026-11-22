package agent

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestUsageHeldInput_CountsAParkedSystemInject is #3257: a system injection
// the usage hold parks reads as held input even when it has no durable form
// (a depth task leaf's turn, enqueued with no serialisable body). The app
// reads UsageHeldInput to show the leaf's conversation as held, not as
// working or waiting for the user. At the lift it runs and no longer reads as
// held.
func TestUsageHeldInput_CountsAParkedSystemInject(t *testing.T) {
	a, _ := forceAgent(t)
	const sk = "test/s/b1"
	testAcct(a).ObserveUsage(weekly(0.98))

	var ran atomic.Int32
	a.Enqueue(Envelope{SessionKey: sk, Text: "task", Inject: &InjectMeta{
		Trigger: "depth", Run: func() { ran.Add(1) },
	}})
	if !waitFor(2*time.Second, func() bool { return a.UsageHeldInput(sk) }) {
		t.Fatal("a depth turn the usage hold parked does not read as held input")
	}
	if ran.Load() != 0 {
		t.Fatal("the depth turn ran during the hold")
	}
	testAcct(a).ObserveUsage(weekly(0.10))
	if !waitFor(3*time.Second, func() bool { return ran.Load() == 1 }) {
		t.Fatal("the held depth turn did not run at the lift")
	}
	if !waitFor(2*time.Second, func() bool { return !a.UsageHeldInput(sk) }) {
		t.Error("UsageHeldInput still true after the depth turn ran")
	}
}
