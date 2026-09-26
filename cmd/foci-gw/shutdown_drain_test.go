package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/memory"
)

// #2059: a wake whose turn is refused because foci is shutting down must stay
// pending so the next process re-fires it; any other outcome consumes it.
func TestWakeTurnDone_KeepsRowOnlyWhenShuttingDown(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		wantPending bool
	}{
		{"ran", nil, false},
		{"failed", errors.New("boom"), false},
		{"refused-shutdown", fmt.Errorf("dispatch: %w", agent.ErrShuttingDown), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs, err := memory.NewReminderStore(filepath.Join(t.TempDir(), "reminders.db"))
			if err != nil {
				t.Fatalf("NewReminderStore: %v", err)
			}
			t.Cleanup(func() { rs.Close() })
			id, err := rs.AddWake("test", "test/main", "wake", "1h")
			if err != nil {
				t.Fatalf("AddWake: %v", err)
			}

			wakeTurnDone(rs, id)(tc.err)

			pending, err := rs.PendingWakes("test")
			if err != nil {
				t.Fatalf("PendingWakes: %v", err)
			}
			if got := len(pending) == 1; got != tc.wantPending {
				t.Fatalf("pending after err=%v: %d row(s), want pending=%v", tc.err, len(pending), tc.wantPending)
			}
		})
	}
}

// #2059: an injection the inbox refuses during shutdown never runs, so its
// completion hook never fires — which is what leaves a wake's row pending.
func TestDeliverToSessionChatThen_RefusedInjectionSkipsHook(t *testing.T) {
	ag := &agent.Agent{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ag.StartInbox(ctx)
	ag.BeginShutdown()

	const sk = "test/main"
	var called atomic.Bool
	deliverToSessionChatThen(ag, ctx, "scheduled_wake", stubConnMgr{}, "test", sk, "wake", "",
		func(error) { called.Store(true) })

	// The inbox is FIFO per session: once a probe behind the wake has been
	// refused, the wake has been processed too.
	pctx, pdone := context.WithTimeout(ctx, 10*time.Second)
	defer pdone()
	if err := ag.EnqueueInjectWait(pctx, sk, "probe", func() {}); !errors.Is(err, agent.ErrShuttingDown) {
		t.Fatalf("probe err = %v, want ErrShuttingDown", err)
	}
	if called.Load() {
		t.Fatal("completion hook fired for an injection refused during shutdown")
	}
}

func TestDescribeBusyTurn_ElapsedFromDispatch(t *testing.T) {
	now := time.Date(2026, 9, 26, 15, 25, 56, 0, time.UTC)
	queued := now.Add(-(3*time.Minute + 26*time.Second))

	// The observed #2059 case: queued 3m26s ago, dispatched 5s ago.
	got := describeBusyTurn("clutch", agent.TurnDetail{
		SessionKey:   "clutch/c1",
		Trigger:      "scheduled_wake",
		StartTime:    queued,
		DispatchedAt: now.Add(-5 * time.Second),
	}, now)
	want := "clutch(session=clutch/c1, trigger=scheduled_wake, elapsed=5s, waited=3m21s before dispatch)"
	if got != want {
		t.Errorf("dispatched turn:\n got %q\nwant %q", got, want)
	}

	// Still waiting: no elapsed at all, just how long it has waited.
	got = describeBusyTurn("clutch", agent.TurnDetail{SessionKey: "clutch/c1", StartTime: queued}, now)
	if !strings.Contains(got, "not dispatched, waiting=3m26s") || strings.Contains(got, "elapsed=") {
		t.Errorf("undispatched turn: got %q", got)
	}

	// Dispatched immediately (API turn): no waited clause.
	got = describeBusyTurn("clutch", agent.TurnDetail{SessionKey: "clutch/c1", StartTime: queued, DispatchedAt: queued, ToolName: "exec"}, now)
	if got != "clutch(session=clutch/c1, tool=exec, elapsed=3m26s)" {
		t.Errorf("immediate turn: got %q", got)
	}
}
