package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/log"
	"foci/internal/memory"
)

// newClosedAfterAddStore returns a reminder store holding one wake row, then
// closed, so every later Dismiss fails ("database is closed").
func newWakeStore(t *testing.T) (*memory.ReminderStore, int64) {
	t.Helper()
	rs, err := memory.NewReminderStore(filepath.Join(t.TempDir(), "reminders.db"))
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })
	id, err := rs.AddWake("test", "test/main", "dismiss me", "24h")
	if err != nil {
		t.Fatalf("AddWake: %v", err)
	}
	return rs, id
}

// TestWakeTurnDone_DismissFailureLogged pins #3080 for the turn-ran path: a
// wake whose turn ran but whose row delete fails would fire again after a
// restart, so the failure is logged at ERROR, naming the wake. Before #3080
// the error was discarded and nothing was logged.
func TestWakeTurnDone_DismissFailureLogged(t *testing.T) {
	c := captureLog(t, log.ERROR)
	rs, id := newWakeStore(t)
	if err := rs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wakeTurnDone(rs, id)(nil)
	want := fmt.Sprintf("wake id=%d: dismiss failed after it ran", id)
	if len(c.Lines(want)) == 0 {
		t.Fatalf("no ERROR line %q for a failed dismiss; captured: %v", want, c.Lines("remind"))
	}
}

// TestWakeTurnDone_DismissSuccessSilent is the control: a successful dismiss
// logs nothing at ERROR and removes the row.
func TestWakeTurnDone_DismissSuccessSilent(t *testing.T) {
	c := captureLog(t, log.ERROR)
	rs, id := newWakeStore(t)
	wakeTurnDone(rs, id)(nil)
	if lines := c.Lines(fmt.Sprintf("wake id=%d: dismiss failed", id)); len(lines) != 0 {
		t.Fatalf("ERROR logged for a successful dismiss: %v", lines)
	}
	if pending, _ := rs.PendingWakes("test"); len(pending) != 0 {
		t.Fatalf("%d pending rows after dismiss, want 0", len(pending))
	}
}

// TestWakeCancel_DismissFailureLogged pins #3080 for the cancel path: a
// cancelled wake whose row delete fails would fire after a restart, so the
// wake goroutine's Done branch logs the failure at ERROR.
func TestWakeCancel_DismissFailureLogged(t *testing.T) {
	c := captureLog(t, log.ERROR)
	rs, id := newWakeStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn, cancelFn := buildWakeScheduler(func() *agent.Agent { return &agent.Agent{} }, rs, "test", ctx, stubConnMgr{})
	// Far enough out that it cannot fire on its own during the test.
	if err := fn(id, 24*time.Hour, "dismiss me", "test/main"); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if err := rs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !cancelFn(id) {
		t.Fatal("cancelFn returned false for a live wake")
	}
	want := fmt.Sprintf("wake id=%d: dismiss failed after it was cancelled", id)
	// The dismiss runs on the wake goroutine; the deadline is a hang guard.
	deadline := time.Now().Add(10 * time.Second)
	for len(c.Lines(want)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no ERROR line %q for a failed dismiss on cancel; captured: %v", want, c.Lines("remind"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}
