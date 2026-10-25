package agent

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/convo"
	"foci/internal/delegator"
	"foci/internal/session"
)

// #2446: a delegated turn's entry writes are split by meaning. The writes that
// record that input ARRIVED (conversation recv row, last_activity_at,
// last_user_activity_at, the OnActivity hooks, the prevRequestTime capture)
// stay at turn entry. The writes that claim a turn RAN (last_cache_touch and
// the app warmth push, lastMessageTime, the turn-end lifecycle hooks) happen
// only once the backend accepted the input: a begin, a fold into the in-flight
// run, or a typed answer. A turn that never reaches the backend — a refused
// keepalive, a system turn abandoned in the begin loop, a human message whose
// write failed, a redelivery no longer pending — leaves no "turn ran" trace.

const entrySplitKey = "test/s"

// entrySplitFixture is a delegated agent over a real session index, seeded
// with known timestamps, and counters on every observable "turn ran" side
// effect.
type entrySplitFixture struct {
	a   *Agent
	idx *session.SessionIndex
	be  *mockBackendDT

	t0 time.Time // seeded last_cache_touch
	a0 time.Time // seeded last_activity_at
	m0 time.Time // seeded lastMessageTime

	completeHooks, endHooks, cacheExpiry, onActivity atomic.Int32
}

func newEntrySplitFixture(t *testing.T) *entrySplitFixture {
	t.Helper()
	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	return newEntrySplitFixtureOn(t, be, be)
}

// newEntrySplitFixtureOn builds the fixture over backend (a mockBackendDT or a
// type embedding one; mock is that embedded mock).
func newEntrySplitFixtureOn(t *testing.T, backend delegator.Delegator, mock *mockBackendDT) *entrySplitFixture {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	f := &entrySplitFixture{idx: idx, be: mock}
	// Whole seconds: the index stores RFC3339 seconds.
	now := time.Now().Truncate(time.Second)
	f.t0 = now.Add(-20 * time.Minute)
	f.a0 = now.Add(-30 * time.Minute)
	f.m0 = now.Add(-40 * time.Minute)

	seedCacheTestRow(idx, entrySplitKey)
	idx.TouchCacheTouch(entrySplitKey, f.t0)
	idx.UpdateActivity(entrySplitKey, f.a0)

	mgr := newMockDelegatedManager(t, backend)
	mgr.SessionIndex = idx
	f.a = &Agent{Model: "test-model", DelegatedManager: mgr, SessionIndex: idx}
	f.a.getSessionMeta(entrySplitKey).lastMessageTime = f.m0
	f.a.SetTurnLifecycleHooks(
		func() { f.completeHooks.Add(1) },
		func() { f.endHooks.Add(1) },
	)
	f.a.SetOnCacheExpiry(func(string, int64) { f.cacheExpiry.Add(1) })
	f.a.OnActivity.Add(func(string) { f.onActivity.Add(1) })
	return f
}

// completeTurns makes every accepted begin finish its turn, as CC would.
func (f *entrySplitFixture) completeTurns() {
	f.be.sendToPaneFn = func(_ context.Context, _ string, h *mockHandler) (*delegator.TurnResult, error) {
		if h != nil && h.OnTurnComplete != nil {
			go h.OnTurnComplete(&delegator.TurnResult{Text: "ok"})
		}
		return nil, nil
	}
}

func (f *entrySplitFixture) cacheTouch(t *testing.T) time.Time {
	t.Helper()
	got, ok := f.idx.LastCacheTouch(entrySplitKey)
	if !ok {
		t.Fatal("last_cache_touch missing")
	}
	return got
}

func (f *entrySplitFixture) lastActivity(t *testing.T) time.Time {
	t.Helper()
	e, err := f.idx.Get(entrySplitKey)
	if err != nil {
		t.Fatalf("index Get: %v", err)
	}
	return e.LastActivityAt
}

func (f *entrySplitFixture) lastMessageTime() time.Time {
	return f.a.getSessionMeta(entrySplitKey).lastMessageTime
}

// assertNoTurnRan checks that none of the "turn ran" writes happened.
func (f *entrySplitFixture) assertNoTurnRan(t *testing.T) {
	t.Helper()
	if got := f.cacheTouch(t); !got.Equal(f.t0) {
		t.Errorf("last_cache_touch = %v, want unchanged %v (no turn reached the backend)", got, f.t0)
	}
	if n := f.cacheExpiry.Load(); n != 0 {
		t.Errorf("cache-expiry pushes = %d, want 0", n)
	}
	if got := f.lastMessageTime(); !got.Equal(f.m0) {
		t.Errorf("lastMessageTime = %v, want unchanged %v", got, f.m0)
	}
	if c, e := f.completeHooks.Load(), f.endHooks.Load(); c != 0 || e != 0 {
		t.Errorf("turn lifecycle hooks fired complete=%d end=%d, want 0 and 0", c, e)
	}
}

// assertTurnRan checks that every "turn ran" write happened.
func (f *entrySplitFixture) assertTurnRan(t *testing.T) {
	t.Helper()
	if got := f.cacheTouch(t); !got.After(f.t0) {
		t.Errorf("last_cache_touch = %v, want advanced past %v", got, f.t0)
	}
	if f.cacheExpiry.Load() == 0 {
		t.Error("no cache-expiry push, want one")
	}
	if got := f.lastMessageTime(); got.Equal(f.m0) {
		t.Errorf("lastMessageTime still %v, want updated", got)
	}
	if c, e := f.completeHooks.Load(), f.endHooks.Load(); c != 1 || e != 1 {
		t.Errorf("turn lifecycle hooks fired complete=%d end=%d, want 1 and 1", c, e)
	}
}

func TestEntrySplit_SkippedKeepaliveLeavesNoTurnTrace(t *testing.T) {
	t.Parallel()
	f := newEntrySplitFixture(t)
	f.be.setTurnInFlight(true) // the backend refuses the keepalive's begin

	err := f.a.HandleMessage(WithTrigger(context.Background(), TriggerKeepalive), entrySplitKey, []string{"ka"}, nil)
	if !errors.Is(err, ErrKeepaliveSkipped) {
		t.Fatalf("HandleMessage = %v, want ErrKeepaliveSkipped", err)
	}
	if n := len(f.be.InjectsRecorded()); n != 1 {
		t.Fatalf("begin attempts = %d, want 1 (premise: the begin was tried and refused)", n)
	}
	f.assertNoTurnRan(t)
	// Keepalive is maintenance, never activity (Dick 2026-10-22: it must not
	// bump the parent's last_activity_at).
	if got := f.lastActivity(t); !got.Equal(f.a0) {
		t.Errorf("last_activity_at = %v, want unchanged %v", got, f.a0)
	}
	if n := f.onActivity.Load(); n != 0 {
		t.Errorf("OnActivity fired %d times, want 0", n)
	}
	// The prevRequestTime capture stays at entry (the resume-missed notice
	// reads it inside Get, before the begin); it reads the unchanged T0.
	if got, ok := f.a.PrevRequestTime(entrySplitKey); !ok || !got.Equal(f.t0) {
		t.Errorf("PrevRequestTime = %v (ok=%v), want %v", got, ok, f.t0)
	}
}

// TestEntrySplit_AbandonedSystemTurnKeepsReceiptOnly drives a scheduled wake
// that waits in the begin loop and is abandoned there (its context ends). It
// arrived, so its receipt writes stay; it never ran, so nothing claims it did.
func TestEntrySplit_AbandonedSystemTurnKeepsReceiptOnly(t *testing.T) {
	t.Parallel()
	f := newEntrySplitFixture(t)
	f.be.setTurnInFlight(true)
	ctx, cancel := context.WithCancel(WithTrigger(context.Background(), "scheduled_wake"))
	defer cancel()
	f.be.waitForTurnFn = func(wctx context.Context) error {
		cancel() // the turn is abandoned while it waits
		<-wctx.Done()
		return wctx.Err()
	}

	err := f.a.HandleMessage(ctx, entrySplitKey, []string{"wake"}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("HandleMessage = %v, want context.Canceled", err)
	}
	f.assertNoTurnRan(t)
	if got := f.lastActivity(t); !got.After(f.a0) {
		t.Errorf("last_activity_at = %v, want advanced past %v (the wake arrived)", got, f.a0)
	}
	if n := f.onActivity.Load(); n != 1 {
		t.Errorf("OnActivity fired %d times, want 1 (receipt)", n)
	}
}

// TestEntrySplit_HumanMessageFailedWriteKeepsReceipt: a voice message whose
// write to the backend fails was still sent by the user — its recv row and
// user activity stay — but no turn ran.
func TestEntrySplit_HumanMessageFailedWriteKeepsReceipt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	if err := convo.InitPerAgent([]string{"test"}, func(string) string { return dbPath }); err != nil {
		t.Fatalf("InitPerAgent: %v", err)
	}
	defer convo.Close()

	f := newEntrySplitFixture(t)
	f.be.sendToPaneFn = func(context.Context, string, *mockHandler) (*delegator.TurnResult, error) {
		return nil, errors.New("pane write failed")
	}

	err := f.a.HandleMessage(WithTrigger(context.Background(), "voice"), entrySplitKey, []string{"hello"}, nil)
	if err == nil {
		t.Fatal("HandleMessage = nil, want the write error")
	}
	f.assertNoTurnRan(t)
	if n := recvRows(t, dbPath); n != 1 {
		t.Errorf("recv rows = %d, want 1 (the user did send it)", n)
	}
	if _, ok := f.idx.LastUserActivity(entrySplitKey); !ok {
		t.Error("last_user_activity_at not stamped, want stamped (the user did act)")
	}
}

func TestEntrySplit_AcceptedBeginStampsTurnRan(t *testing.T) {
	t.Parallel()
	for _, trigger := range []string{"scheduled_wake", TriggerKeepalive, "voice"} {
		t.Run(trigger, func(t *testing.T) {
			f := newEntrySplitFixture(t)
			f.completeTurns()
			if err := f.a.HandleMessage(WithTrigger(context.Background(), trigger), entrySplitKey, []string{"go"}, nil); err != nil {
				t.Fatalf("HandleMessage: %v", err)
			}
			f.assertTurnRan(t)
			activityMoved := f.lastActivity(t).After(f.a0)
			if wantMoved := trigger != TriggerKeepalive; activityMoved != wantMoved {
				t.Errorf("last_activity_at advanced=%v, want %v", activityMoved, wantMoved)
			}
		})
	}
}

// TestEntrySplit_FoldStampsTurnRan: a message folded into the in-flight run
// reached the model, so it counts as a turn that ran.
func TestEntrySplit_FoldStampsTurnRan(t *testing.T) {
	t.Parallel()
	f := newEntrySplitFixture(t)
	f.be.setTurnInFlight(true)
	if err := f.a.HandleMessage(WithTrigger(context.Background(), "voice"), entrySplitKey, []string{"also"}, nil); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if n := len(f.be.InjectsRecorded()); n != 1 || f.be.InjectsRecorded()[0].Source != delegator.SourceUser {
		t.Fatalf("injects = %+v, want one SourceUser fold (premise)", f.be.InjectsRecorded())
	}
	f.assertTurnRan(t)
}

// TestEntrySplit_TypedAnswerStampsTurnRan: a message consumed as the answer
// to a pending backend question resumes the backend's running turn, so it
// counts as a turn that ran.
func TestEntrySplit_TypedAnswerStampsTurnRan(t *testing.T) {
	t.Parallel()
	be := &questionBackendDT{mockBackendDT: mockBackendDT{sessionFile: "/tmp/s.jsonl"}, pendingQuestion: "q1"}
	f := newEntrySplitFixtureOn(t, be, &be.mockBackendDT)
	if err := f.a.HandleMessage(WithTrigger(context.Background(), "voice"), entrySplitKey, []string{"yes"}, nil); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if be.answeredReqID != "q1" {
		t.Fatalf("answered %q, want q1 (premise: the text answered the question)", be.answeredReqID)
	}
	f.assertTurnRan(t)
}

// TestEntrySplit_StaleRedeliveryLeavesNoTurnTrace: a redelivery whose input
// was dropped while it queued returns without reaching the backend.
func TestEntrySplit_StaleRedeliveryLeavesNoTurnTrace(t *testing.T) {
	t.Parallel()
	f := newEntrySplitFixture(t)
	ctx := WithRedelivery(WithTrigger(context.Background(), TriggerRedelivery),
		Redelivery{Input: delegator.PendingInput{ID: "gone", Text: "old"}, Reason: "restart"})
	if err := f.a.HandleMessage(ctx, entrySplitKey, []string{"old"}, nil); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if n := len(f.be.InjectsRecorded()); n != 0 {
		t.Fatalf("injects = %d, want 0 (premise: the stale redelivery is skipped)", n)
	}
	f.assertNoTurnRan(t)
}

func recvRows(t *testing.T, dbPath string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open convo DB: %v", err)
	}
	defer db.Close() // nolint:errcheck
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE direction = 'recv'`).Scan(&n); err != nil {
		t.Fatalf("count recv rows: %v", err)
	}
	return n
}
