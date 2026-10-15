package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/log"
	"foci/internal/memory"
)

// TestWakeScheduler_FiredFromTimerUntilTurnRuns pins the fired overlay's
// lifetime (#2233): from the moment the timer fires until the wake's turn has
// run, the store reports the wake fired and the row stays; once the turn runs
// (here: the inbox starts and drains the parked injection), both clear.
func TestWakeScheduler_FiredFromTimerUntilTurnRuns(t *testing.T) {
	rs, err := memory.NewReminderStore(filepath.Join(t.TempDir(), "reminders.db"))
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })

	// The inbox is deliberately NOT started: the fired wake's injection parks
	// on the session channel, so "fired" persists deterministically.
	ag := &agent.Agent{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn, _ := buildWakeScheduler(func() *agent.Agent { return ag }, rs, "test", ctx, stubConnMgr{})

	id, err := rs.AddWake("test", "test/main", "fire me", "24h")
	if err != nil {
		t.Fatalf("AddWake: %v", err)
	}
	if err := fn(id, 30*time.Millisecond, "fire me", "test/main"); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	// Wait for the timer to fire and the claim to stamp the overlay.
	sk, ok := "", false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sk, ok = rs.FiredWake(id); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ok || sk != "test/main" {
		t.Fatalf("FiredWake = (%q, %v) after firing, want (test/main, true)", sk, ok)
	}
	if wakes, _ := rs.PendingWakes("test"); len(wakes) != 1 {
		t.Fatalf("pending rows = %d before the turn ran, want 1", len(wakes))
	}

	// The turn runs (inbox starts, drains the injection, the turn completes on
	// the bare agent): the row is dismissed and the fired marker goes with it.
	ag.StartInbox(ctx)
	rowDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(rowDeadline) {
		if wakes, _ := rs.PendingWakes("test"); len(wakes) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if wakes, _ := rs.PendingWakes("test"); len(wakes) != 0 {
		t.Fatalf("wake row still pending after the turn ran: %d", len(wakes))
	}
	if _, ok := rs.FiredWake(id); ok {
		t.Fatal("wake still reads as fired after its turn ran")
	}
}

// TestWakeScheduler_FiredClearedWhenInjectRefused pins the refusal arm: a
// fired wake whose injection the inbox declines to run at shutdown drops its
// fired marker (the row stays pending for the next process, #2059). The fire
// is observed FIRST — no inbox worker exists yet, so nothing can clear the
// marker — which is what fail-arms the cleared wait: without it the poll
// could exit before the timer has fired at all and prove nothing.
func TestWakeScheduler_FiredClearedWhenInjectRefused(t *testing.T) {
	rs, err := memory.NewReminderStore(filepath.Join(t.TempDir(), "reminders.db"))
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })

	// No inbox yet: the fired wake's injection parks on the session channel,
	// so the fired marker stays observable.
	ag := &agent.Agent{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn, _ := buildWakeScheduler(func() *agent.Agent { return ag }, rs, "test", ctx, stubConnMgr{})

	id, err := rs.AddWake("test", "test/main", "refuse me", "24h")
	if err != nil {
		t.Fatalf("AddWake: %v", err)
	}
	if err := fn(id, 30*time.Millisecond, "refuse me", "test/main"); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	// The timer fires; with no worker running the marker can only be set.
	sk, ok := "", false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sk, ok = rs.FiredWake(id); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ok || sk != "test/main" {
		t.Fatalf("FiredWake = (%q, %v) after firing, want (test/main, true) — the fire was never observed, the refusal below would prove nothing", sk, ok)
	}

	// Shutdown, then the worker: it must refuse the parked injection, and the
	// refusal hook must clear the marker.
	ag.BeginShutdown()
	ag.StartInbox(ctx)

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok = rs.FiredWake(id); !ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := rs.FiredWake(id); ok {
		t.Fatal("fired marker survived a shutdown refusal — the next process re-fires the row, nothing is waiting")
	}
	if wakes, _ := rs.PendingWakes("test"); len(wakes) != 1 {
		t.Fatalf("pending rows = %d after the refusal, want 1 (row survives for restart)", len(wakes))
	}
}

// TestEnqueueInject_FullInboxFiresRefusedHook pins the enqueue-time refusal
// arm of the same contract: when the inbox declines the envelope outright (the
// session's channel is full), enqueueInject fires the Refused hook
// synchronously, so a caller's runtime state — a wake's fired marker — does
// not linger behind a drop. The wake timer itself cannot be observed between
// fire and drop (the claim marks the overlay and the refused enqueue clears it
// in the same goroutine), so the arm is pinned at its own seam.
func TestEnqueueInject_FullInboxFiresRefusedHook(t *testing.T) {
	ag := &agent.Agent{}
	// No inbox worker: every enqueue lands on the (bounded) channel. Fill it
	// completely first, so the injected turn's own enqueue is refused.
	for i := 0; i < 64; i++ {
		if !ag.Enqueue(agent.Envelope{SessionKey: "test/main", Inject: &agent.InjectMeta{Trigger: "fill", Run: func() {}}}) {
			t.Fatalf("fill enqueue %d refused early", i)
		}
	}

	var refused atomic.Bool
	enqueueInject(ag, "test/main", "scheduled_wake", "", func() {}, onInjectRefused(func() { refused.Store(true) }))
	if !refused.Load() {
		t.Fatal("full-inbox refusal did not fire the Refused hook — the fired marker would linger behind a dropped inject (#2233)")
	}
}

// TestWakeScheduler_CancelDuringFireSeesFired pins the claim protocol with the
// overlay in place: once a wake has fired, a cancel finds no live timer (it is
// not cancellable, #2036) and the remind tool's fired reporting has the
// marker it needs — no "no timer, not fired" gap.
func TestWakeScheduler_CancelDuringFireSeesFired(t *testing.T) {
	rs, err := memory.NewReminderStore(filepath.Join(t.TempDir(), "reminders.db"))
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })

	ag := &agent.Agent{} // no inbox: the fired wake parks indefinitely
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn, cancelFn := buildWakeScheduler(func() *agent.Agent { return ag }, rs, "test", ctx, stubConnMgr{})

	id, err := rs.AddWake("test", "test/main", "no gap", "24h")
	if err != nil {
		t.Fatalf("AddWake: %v", err)
	}
	if err := fn(id, 30*time.Millisecond, "no gap", "test/main"); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	var ok bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok = rs.FiredWake(id); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ok {
		t.Fatal("fired wake has no marker — the cancel-vs-fire gap is observable")
	}
	if cancelFn(id) {
		t.Fatal("cancel of a fired wake reported success (#2036 claim rule)")
	}
}

// TestWakeScheduler_FiringIsLogged pins the operator-visible half of a wake
// firing: the scheduler logs an Info line naming the wake the moment its timer
// fires. The fire is observed first through the overlay marker (which also
// fail-arms the wait — on a tree without the marker there is nothing to wait
// for, and the log assertion could pass before the timer fired at all), and
// the line is then asserted through a log tap. Without the line, a fired wake
// leaves no trace in foci.log at all.
func TestWakeScheduler_FiringIsLogged(t *testing.T) {
	c := captureLog(t, log.INFO)
	rs, err := memory.NewReminderStore(filepath.Join(t.TempDir(), "reminders.db"))
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })

	// No inbox: the fired wake's injection parks on the session channel, so
	// the fired state is stable while the log line is asserted.
	ag := &agent.Agent{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn, _ := buildWakeScheduler(func() *agent.Agent { return ag }, rs, "test", ctx, stubConnMgr{})

	msg := runToken("firelog")
	id, err := rs.AddWake("test", "test/main", msg, "24h")
	if err != nil {
		t.Fatalf("AddWake: %v", err)
	}
	if err := fn(id, 30*time.Millisecond, msg, "test/main"); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	ok := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok = rs.FiredWake(id); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ok {
		t.Fatal("wake never fired — the log assertion below would prove nothing")
	}

	// The line follows the claim in the same goroutine; the bound only covers
	// scheduling slack.
	lineDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(lineDeadline) {
		if len(c.Lines("firing wake id=", msg)) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no \"firing wake\" log line for id=%d — a fired wake must leave an operator-visible record:\n%s", id, c.String())
}

// TestWakeTurnDone_ShutdownRefusalClearsFiredOverlay pins the completion
// hook's shutdown arm: a wake whose turn was refused mid-drain keeps its row
// for the next process (#2059) AND drops its fired marker — the next process
// re-marks it when its restored timer fires, so the overlay must not survive
// the refusal.
func TestWakeTurnDone_ShutdownRefusalClearsFiredOverlay(t *testing.T) {
	rs, err := memory.NewReminderStore(filepath.Join(t.TempDir(), "reminders.db"))
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })
	id, err := rs.AddWake("test", "test/main", "wake", "1h")
	if err != nil {
		t.Fatalf("AddWake: %v", err)
	}
	rs.MarkWakeFired(id, "test/main")

	wakeTurnDone(rs, id)(fmt.Errorf("dispatch: %w", agent.ErrShuttingDown))

	if _, ok := rs.FiredWake(id); ok {
		t.Fatal("fired marker survived a shutdown refusal — the row stays pending and the next process re-marks it when it re-fires")
	}
	if wakes, _ := rs.PendingWakes("test"); len(wakes) != 1 {
		t.Fatalf("pending rows = %d, want 1 (row survives for restart, #2059)", len(wakes))
	}
}
