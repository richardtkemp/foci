package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/platform"
	"foci/internal/turnevent"
)

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// acquireAsync starts an acquire and returns a channel closed once it holds a slot.
func acquireAsync(s *spawnSlots) <-chan struct{} {
	got := make(chan struct{})
	go func() {
		if s.acquire(context.Background()) == nil {
			close(got)
		}
	}()
	return got
}

func isClosed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// #2722: a raise starts queued waiters at once, in FIFO order.
func TestSpawnSlotsRaiseWakesWaitersFIFO(t *testing.T) {
	t.Parallel()
	s := newSpawnSlots(1)
	if err := s.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	var order []int
	done := make(chan int, 3)
	for i := 0; i < 3; i++ {
		i := i
		go func() {
			_ = s.acquire(context.Background())
			done <- i
		}()
		waitFor(t, "waiter to queue", func() bool { _, w, _ := s.counts(); return w == i+1 })
	}
	s.setLimit(3) // room for two of the three waiters, without any release
	for len(order) < 2 {
		select {
		case i := <-done:
			order = append(order, i)
		case <-time.After(5 * time.Second):
			t.Fatalf("raise woke %d waiters, want 2", len(order))
		}
	}
	if held, waiting, _ := s.counts(); held != 3 || waiting != 1 {
		t.Fatalf("held=%d waiting=%d, want 3 and 1", held, waiting)
	}
	s.release()
	order = append(order, <-done)
	// Grants are FIFO; the goroutines' sends to done may still reorder the
	// first two, but the last one served must be the last to queue.
	if order[2] != 2 {
		t.Fatalf("service order %v, want waiter 2 served last", order)
	}
}

// #2722: lowering the limit keeps every held slot and holds new acquisitions
// back until the count drops below the new limit.
func TestSpawnSlotsLowerHoldsNewUntilBelow(t *testing.T) {
	t.Parallel()
	s := newSpawnSlots(3)
	for i := 0; i < 3; i++ {
		if err := s.acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	s.setLimit(1)
	if held, _, _ := s.counts(); held != 3 {
		t.Fatalf("lowering changed held to %d, want 3 (running spawns are kept)", held)
	}
	got := acquireAsync(s)
	waitFor(t, "waiter to queue", func() bool { _, w, _ := s.counts(); return w == 1 })
	s.release() // 2 held, limit 1: still full
	s.release() // 1 held: still full
	time.Sleep(20 * time.Millisecond)
	if isClosed(got) {
		t.Fatal("a new acquisition started while held >= the lowered limit")
	}
	s.release() // 0 held: room for one
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never started after the count dropped below the limit")
	}
	if held, waiting, _ := s.counts(); held != 1 || waiting != 0 {
		t.Fatalf("held=%d waiting=%d, want 1 and 0", held, waiting)
	}
}

// A cancelled wait leaves the queue and takes no slot; acquire never jumps a
// queued waiter.
func TestSpawnSlotsCancelAndNoQueueJump(t *testing.T) {
	t.Parallel()
	s := newSpawnSlots(1)
	_ = s.acquire(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.acquire(ctx) }()
	waitFor(t, "waiter to queue", func() bool { _, w, _ := s.counts(); return w == 1 })
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("cancelled acquire reported a slot")
	}
	if held, waiting, _ := s.counts(); held != 1 || waiting != 0 {
		t.Fatalf("held=%d waiting=%d after cancel, want 1 and 0", held, waiting)
	}
	first := acquireAsync(s)
	waitFor(t, "waiter to queue", func() bool { _, w, _ := s.counts(); return w == 1 })
	s.setLimit(1) // no room: a no-op
	s.release()
	<-first
	if held, _, _ := s.counts(); held != 1 {
		t.Fatalf("held=%d, want 1", held)
	}
}

// #2722 end to end: background spawns queued behind a full limit start when
// SetMaxConcurrent raises it, with no slot released.
func TestSpawnerSetMaxConcurrentRaiseStartsQueued(t *testing.T) {
	t.Parallel()
	var running, maxRunning int32
	release := make(chan struct{})
	notifier := NewAsyncNotifier(func(string, string, string, string, *AgentOrigin) {})
	spawner := NewSpawner(SpawnDeps{
		Sessions:       &mockSessionBrancher{},
		AgentID:        "live-slots",
		FallbackModel:  "anthropic/claude-haiku-4-5",
		FallbackFormat: "anthropic",
		MaxInherit:     1,
		Notifier:       notifier,
	}, func() SpawnAgent {
		return &slotSpawnAgent{running: &running, max: &maxRunning, release: release}
	})
	tool := spawner.SpawnTool()
	ctx := WithSessionKey(context.Background(), "test/imain")
	params, _ := json.Marshal(map[string]string{"prompt": "task", "context": "clone"})

	// The first spawn takes the only slot; the next two block in Execute.
	acks := make(chan string, 3)
	for i := 0; i < 3; i++ {
		go func() {
			r, err := tool.Execute(ctx, params)
			if err != nil {
				acks <- "error: " + err.Error()
				return
			}
			acks <- r.Text
		}()
	}
	waitFor(t, "one running, two queued", func() bool {
		_, w, _ := spawner.slots.counts()
		return atomic.LoadInt32(&running) == 1 && w == 2
	})

	spawner.SetMaxConcurrent(3)
	waitFor(t, "queued spawns to start after the raise", func() bool { return atomic.LoadInt32(&running) == 3 })
	for i := 0; i < 3; i++ {
		if a := <-acks; !strings.Contains(a, "Spawn started in background") {
			t.Fatalf("ack %q, want a background ack", a)
		}
	}

	// Lower to 1 while 3 run: none is stopped, and a fourth waits until the
	// count drops below 1.
	spawner.SetMaxConcurrent(1)
	if r := atomic.LoadInt32(&running); r != 3 {
		t.Fatalf("lowering left %d running, want all 3", r)
	}
	go func() { r, _ := tool.Execute(ctx, params); acks <- r.Text }()
	waitFor(t, "fourth to queue", func() bool { _, w, _ := spawner.slots.counts(); return w == 1 })
	time.Sleep(20 * time.Millisecond)
	if len(acks) != 0 {
		t.Fatal("a fourth spawn started above the lowered limit")
	}
	close(release)
	select {
	case a := <-acks:
		if !strings.Contains(a, "Spawn started in background") {
			t.Fatalf("fourth ack %q, want a background ack", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fourth spawn never started once the running ones ended")
	}
	if m := atomic.LoadInt32(&maxRunning); m != 3 {
		t.Fatalf("max running %d, want 3", m)
	}
}

// slotSpawnAgent counts concurrent runs and blocks each until release closes.
type slotSpawnAgent struct {
	running, max *int32
	release      chan struct{}
}

func (a *slotSpawnAgent) HandleMessage(ctx context.Context, _ string, _ []string, _ []platform.Attachment) error {
	n := atomic.AddInt32(a.running, 1)
	for {
		m := atomic.LoadInt32(a.max)
		if n <= m || atomic.CompareAndSwapInt32(a.max, m, n) {
			break
		}
	}
	defer atomic.AddInt32(a.running, -1)
	select {
	case <-a.release:
	case <-ctx.Done():
	}
	turnevent.Emit(ctx, turnevent.TurnComplete{FinalText: "ok"})
	return nil
}
