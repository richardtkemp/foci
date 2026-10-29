package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/memory"
	"foci/internal/tools"
)

// stubWakeOwner replaces the depth manager's owner walk for the test.
func stubWakeOwner(t *testing.T, fn func(ctx context.Context, sk string) (tools.SpawnRoute, bool)) {
	t.Helper()
	prev := awaitWakeOwner
	awaitWakeOwner = fn
	t.Cleanup(func() { awaitWakeOwner = prev })
}

// waitFired polls until wake id is marked fired and returns the session it was
// stamped with. The deadline is a hang guard.
func waitFired(t *testing.T, rs *memory.ReminderStore, id int64) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sk, ok := rs.FiredWake(id); ok {
			return sk
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("wake %d never fired", id)
	return ""
}

// #2647: a wake set from a depth leaf that has since closed fires into the
// leaf's owner (the walk spawn results take), not into the leaf.
func TestWakeScheduler_ClosedLeafWakeFiresIntoOwner(t *testing.T) {
	const leaf, root = "test/c1/b1", "test/c1"
	stubWakeOwner(t, func(_ context.Context, sk string) (tools.SpawnRoute, bool) {
		if sk == leaf {
			return tools.SpawnRoute{Owner: root, Via: `depth leaf "Alpha" (test/c1/b1)`}, true
		}
		return tools.SpawnRoute{Owner: sk}, true
	})
	rs, err := memory.NewReminderStore(filepath.Join(t.TempDir(), "reminders.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rs.Close() })
	// No inbox: the injection parks, so the fired stamp stays observable.
	ag := &agent.Agent{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn, _ := buildWakeScheduler(func() *agent.Agent { return ag }, rs, "test", ctx, stubConnMgr{})

	id, err := rs.AddWake("test", leaf, "check the build", "24h")
	if err != nil {
		t.Fatal(err)
	}
	if err := fn(id, 0, "check the build", leaf); err != nil {
		t.Fatal(err)
	}
	if sk := waitFired(t, rs, id); sk != root {
		t.Fatalf("wake fired into %s, want the leaf's owner %s", sk, root)
	}
}

// #2647 req 3: the wake carries the leaf it came from; a wake that stays on
// its own session is unchanged (req 1, 5).
func TestWakeDelivery_MarksTheLeaf(t *testing.T) {
	target, body := wakeDelivery("test/c1/b1", "check the build", tools.SpawnRoute{Owner: "test/c1", Via: `depth leaf "Alpha" (test/c1/b1)`})
	if target != "test/c1" || !strings.Contains(body, "[REPARENTED WAKE]") || !strings.Contains(body, `"Alpha"`) || !strings.HasSuffix(body, "check the build") {
		t.Errorf("reparented: target %q body %q", target, body)
	}
	for _, r := range []tools.SpawnRoute{{Owner: "test/c1/b1", Via: `depth leaf "Alpha" (test/c1/b1)`}, {}} {
		if target, body := wakeDelivery("test/c1/b1", "check the build", r); target != "test/c1/b1" || body != "check the build" {
			t.Errorf("own session (%+v): target %q body %q", r, target, body)
		}
	}
}

// A wake held by its route (the closed leaf's report has not reached its
// parent's fan-in) can still be cancelled: the cancel reports success and the
// row is dismissed without the wake ever firing.
func TestWakeScheduler_HeldWakeCancellable(t *testing.T) {
	waiting := make(chan struct{})
	stubWakeOwner(t, func(ctx context.Context, sk string) (tools.SpawnRoute, bool) {
		close(waiting)
		<-ctx.Done()
		return tools.SpawnRoute{Owner: sk, Hold: true}, false
	})
	rs, err := memory.NewReminderStore(filepath.Join(t.TempDir(), "reminders.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rs.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn, cancelFn := buildWakeScheduler(func() *agent.Agent { return &agent.Agent{} }, rs, "test", ctx, stubConnMgr{})

	id, err := rs.AddWake("test", "test/c1/b1", "held", "24h")
	if err != nil {
		t.Fatal(err)
	}
	if err := fn(id, 0, "held", "test/c1/b1"); err != nil {
		t.Fatal(err)
	}
	<-waiting
	if !cancelFn(id) {
		t.Fatal("cancel of a held wake = false, want true")
	}
	waitWakeRowsDismissed(t, rs, "test")
	if _, ok := rs.FiredWake(id); ok {
		t.Error("a held wake that was cancelled reads as fired")
	}
}

// restorePendingWakes re-arms the stored wakes (moved out of
// buildWakeScheduler so the depth manager exists before an overdue wake
// fires, #2647).
func TestRestorePendingWakes_FiresOverdue(t *testing.T) {
	rs, err := memory.NewReminderStore(filepath.Join(t.TempDir(), "reminders.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rs.Close() })
	id, err := rs.AddWake("test", "test/main", "overdue", "1s")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn, _ := buildWakeScheduler(func() *agent.Agent { return &agent.Agent{} }, rs, "test", ctx, stubConnMgr{})
	restorePendingWakes(rs, "test", fn)
	if sk := waitFired(t, rs, id); sk != "test/main" {
		t.Fatalf("restored wake fired into %s, want test/main", sk)
	}
}
