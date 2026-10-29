package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

// wakeAwait is what AwaitWakeOwner returned.
type wakeAwait struct {
	r  SpawnRoute
	ok bool
}

// startAwaitWake runs AwaitWakeOwner for key in the background; the channel
// gets its result once it returns.
func startAwaitWake(ctx context.Context, m *DepthManager, key string) <-chan wakeAwait {
	ch := make(chan wakeAwait, 1)
	go func() {
		r, ok := m.AwaitWakeOwner(ctx, key)
		ch <- wakeAwait{r, ok}
	}()
	return ch
}

// notYet fails if the await has returned (it must still be held). The wait is
// the window in which a wrongly unheld await would have returned.
func notYet(t *testing.T, ch <-chan wakeAwait) {
	t.Helper()
	select {
	case got := <-ch:
		t.Fatalf("wake delivered while its route holds: %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

// gotAwait waits for the await to return; the deadline is a hang guard.
func gotAwait(t *testing.T, ch <-chan wakeAwait) wakeAwait {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("wake still held after its handover point")
		return wakeAwait{}
	}
}

// #2647 req 1 and 5: a wake set from an open leaf, or from a session that is
// not a depth node, fires into that session at once.
func TestWakeOwnerOpenLeafAndNonDepth(t *testing.T) {
	h := newDepthHarness(t, nil)
	kids := h.fanout(t, depthRoot, "Alpha", "Beta")
	for _, key := range []string{kids[0], "test/c99"} {
		r, ok := h.m.AwaitWakeOwner(context.Background(), key)
		if !ok || r.Owner != key || r.Hold {
			t.Errorf("wake from %s: route %+v ok=%v, want its own session", key, r, ok)
		}
		if got := h.m.WakeOwner(key); got != key {
			t.Errorf("WakeOwner(%s) = %s, want itself", key, got)
		}
	}
}

// #2647 req 2-4 (the ticket's worked example): leaf L reports while a sibling
// is open. Its wake waits (and is still listed on L) until the root's fan-in
// has taken L's report; then it goes to the root, marked as from L, and the
// root's indicator owns it.
func TestWakeFromReportedLeafHeldUntilFanInThenRoot(t *testing.T) {
	h := newDepthHarness(t, nil)
	kids := h.fanout(t, depthRoot, "Alpha", "Beta")
	if err := h.report(t, kids[0], "alpha settled"); err != nil {
		t.Fatal(err)
	}
	ch := startAwaitWake(context.Background(), h.m, kids[0])
	notYet(t, ch)
	if got := h.m.WakeOwner(kids[0]); got != kids[0] {
		t.Errorf("held wake owned by %s, want the leaf %s until the fan-in", got, kids[0])
	}

	if err := h.report(t, kids[1], "beta settled"); err != nil {
		t.Fatal(err)
	}
	got := gotAwait(t, ch)
	if !got.ok || got.r.Owner != depthRoot || got.r.Hold {
		t.Fatalf("after fan-in: %+v, want the root", got)
	}
	if !strings.Contains(got.r.Via, `"Alpha"`) || !strings.Contains(got.r.Via, kids[0]) {
		t.Errorf("Via = %q, want the leaf's title and key", got.r.Via)
	}
	if note := ReparentedWakeNote(got.r.Via); !strings.Contains(note, `"Alpha"`) {
		t.Errorf("note lacks the leaf title: %q", note)
	}
	if o := h.m.WakeOwner(kids[0]); o != depthRoot {
		t.Errorf("WakeOwner after fan-in = %s, want the root", o)
	}
}

// The walk goes node by node to the root, as for spawns.
func TestWakeFromDeepLeafWalksToRoot(t *testing.T) {
	h := newDepthHarness(t, nil)
	a := h.fanout(t, depthRoot, "Alpha")[0]
	a1 := h.fanout(t, a, "Alpha one")[0]
	if err := h.report(t, a1, "a1 done"); err != nil {
		t.Fatal(err)
	}
	if o := h.m.WakeOwner(a1); o != a {
		t.Errorf("after a1's fan-in: owner %s, want its parent %s", o, a)
	}
	if err := h.report(t, a, "a done"); err != nil {
		t.Fatal(err)
	}
	r, ok := h.m.AwaitWakeOwner(context.Background(), a1)
	if !ok || r.Owner != depthRoot || !strings.Contains(r.Via, `"Alpha one"`) {
		t.Fatalf("route %+v ok=%v, want the root, from Alpha one", r, ok)
	}
}

// A held wake that is cancelled stops waiting.
func TestHeldWakeCancelled(t *testing.T) {
	h := newDepthHarness(t, nil)
	kids := h.fanout(t, depthRoot, "Alpha", "Beta")
	if err := h.report(t, kids[0], "alpha settled"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := startAwaitWake(ctx, h.m, kids[0])
	notYet(t, ch)
	cancel()
	if got := gotAwait(t, ch); got.ok {
		t.Fatalf("cancelled held wake reported deliverable: %+v", got)
	}
}

// Depth mode off (the #2628 rule): a held wake is released to the first open
// node up the tree when depth mode is turned off.
func TestHeldWakeReleasedWhenDepthTurnedOff(t *testing.T) {
	h := newDepthHarness(t, nil)
	kids := h.fanout(t, depthRoot, "Alpha", "Beta")
	if err := h.report(t, kids[0], "alpha settled"); err != nil {
		t.Fatal(err)
	}
	ch := startAwaitWake(context.Background(), h.m, kids[0])
	notYet(t, ch)
	h.off.Store(true)
	h.m.OnDisable()
	if got := gotAwait(t, ch); !got.ok || got.r.Owner != depthRoot {
		t.Fatalf("after depth off: %+v, want the root", got)
	}
}
