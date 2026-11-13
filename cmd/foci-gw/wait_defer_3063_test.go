package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/defersend"
	"foci/internal/log"
	"foci/internal/timeutil"
)

// #3063: a deferred send's row must outlive its hand-off to the session's
// in-memory inbox. It is deleted only once the delivery has run, so a record
// queued behind a long turn survives a restart, and a record that has run is
// never run again.

// blockSession parks sk's inbox worker inside an injection until the returned
// release func is called (also called at cleanup), so anything queued after
// it waits, exactly as it would behind a long turn.
func blockSession(t *testing.T, d httpHandlerDeps, sk string) (release func()) {
	t.Helper()
	inst, _ := d.agents.Get(testAgentID)
	started, done := make(chan struct{}), make(chan struct{})
	if !inst.ag.Enqueue(agent.Envelope{SessionKey: sk, Inject: &agent.InjectMeta{Trigger: "user", Run: func() {
		close(started)
		<-done
	}}}) {
		t.Fatal("blocker refused")
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("blocker did not start")
	}
	var closed bool
	release = func() {
		if !closed {
			closed = true
			close(done)
		}
	}
	t.Cleanup(release)
	return release
}

// enqueueReadySend stores a send that is deliverable on the next sweep (its
// deadline has passed) and returns its id.
func enqueueReadySend(t *testing.T, store *defersend.Store, sk, text string) int64 {
	t.Helper()
	now := timeutil.Now()
	id, _, err := store.Enqueue(defersend.Record{
		AgentID: testAgentID, SessionKey: sk, Text: text, Policy: "fallback",
		WaitWarm: "1h", CreatedAt: now.Add(-2 * time.Hour), DeadlineAt: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func storeRows(t *testing.T, store *defersend.Store) []defersend.Record {
	t.Helper()
	rows, err := store.All()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestSweep_QueuedSendSurvivesRestart is the #3063 regression test: block the
// session worker, sweep a ready record, "restart" (a new gateway and a new
// sweeper on the same store) — the record still exists and runs there.
func TestSweep_QueuedSendSurvivesRestart(t *testing.T) {
	t.Parallel()
	d1, mock1 := httpTestSetup(t, httpTestOpts{})
	store := withDeferStore(t, &d1)
	blockSession(t, d1, testSessionKey)
	mock1.entered = make(chan string, 1)

	enqueueReadySend(t, store, testSessionKey, "HELD ACROSS RESTART")
	sweepFor(d1, store).sweep()

	rows := storeRows(t, store)
	if len(rows) != 1 {
		t.Fatalf("after the sweep queued the send behind a busy session: rows=%d, want 1 (deleted at in-memory enqueue)", len(rows))
	}
	if !rows[0].StartedAt.IsZero() {
		t.Fatalf("queued, not yet run, record is marked started at %v", rows[0].StartedAt)
	}
	select {
	case x := <-mock1.entered:
		t.Fatalf("send ran behind the blocker: %q", x)
	default:
	}

	// Restart: the old process is gone (its blocker never releases here), a
	// new one opens the same store.
	d2, mock2 := httpTestSetup(t, httpTestOpts{})
	d2.deferStore = store
	mock2.entered = make(chan string, 1)
	sweepFor(d2, store).sweep()
	select {
	case text := <-mock2.entered:
		if !strings.HasSuffix(text, "HELD ACROSS RESTART") {
			t.Errorf("delivered text = %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the record queued before the restart did not run after it")
	}
	if !eventually(func() bool { return len(storeRows(t, store)) == 0 }) {
		t.Errorf("record not deleted after it ran: %+v", storeRows(t, store))
	}
}

// TestSweep_InFlightRecordNotRedelivered: the row now stays while the send
// waits in the inbox, so later sweeps in the same process must not hand it
// over again.
func TestSweep_InFlightRecordNotRedelivered(t *testing.T) {
	t.Parallel()
	d, mock := httpTestSetup(t, httpTestOpts{})
	store := withDeferStore(t, &d)
	release := blockSession(t, d, testSessionKey)
	mock.entered = make(chan string, 4)

	enqueueReadySend(t, store, testSessionKey, "ONCE ONLY")
	sw := sweepFor(d, store)
	sw.sweep()
	sw.sweep()
	sw.sweep()
	release()

	select {
	case <-mock.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("send never ran")
	}
	if !eventually(func() bool { return len(storeRows(t, store)) == 0 }) {
		t.Fatalf("record not deleted after it ran: %+v", storeRows(t, store))
	}
	sw.sweep()
	if n := len(mock.snapshot()); n != 1 {
		t.Fatalf("backend called %d times, want 1 (a queued record was handed over again)", n)
	}
}

// TestSweep_StartedRecordNotRerunAfterRestart: a record whose delivery a
// previous process started but never confirmed finished is not run again —
// it may already have had its effect — and is dropped, not silently kept.
func TestSweep_StartedRecordNotRerunAfterRestart(t *testing.T) {
	t.Parallel()
	d, mock := httpTestSetup(t, httpTestOpts{})
	store := withDeferStore(t, &d)
	id := enqueueReadySend(t, store, testSessionKey, "ALREADY STARTED")
	if err := store.MarkStarted(id, timeutil.Now()); err != nil {
		t.Fatal(err)
	}
	sweepFor(d, store).sweep()
	if rows := storeRows(t, store); len(rows) != 0 {
		t.Errorf("interrupted record kept: %+v", rows)
	}
	if calls := mock.snapshot(); len(calls) != 0 {
		t.Errorf("interrupted record ran again: %+v", calls)
	}
}

// TestSweep_ShutdownRefusalKeepsRecord: a queued send the inbox refuses to
// run because foci is shutting down keeps its row, unstarted, for the next
// process.
func TestSweep_ShutdownRefusalKeepsRecord(t *testing.T) {
	t.Parallel()
	d, mock := httpTestSetup(t, httpTestOpts{})
	store := withDeferStore(t, &d)
	release := blockSession(t, d, testSessionKey)
	id := enqueueReadySend(t, store, testSessionKey, "REFUSED AT SHUTDOWN")
	sw := sweepFor(d, store)
	sw.sweep()
	if !sw.inFlight(id) {
		t.Fatal("queued record not tracked as in flight")
	}

	inst, _ := d.agents.Get(testAgentID)
	inst.ag.BeginShutdown()
	release()
	if !eventually(func() bool { return !sw.inFlight(id) }) {
		t.Fatal("refused record still marked in flight")
	}
	rows := storeRows(t, store)
	if len(rows) != 1 || !rows[0].StartedAt.IsZero() {
		t.Fatalf("rows = %+v, want the one record, not started", rows)
	}
	if calls := mock.snapshot(); len(calls) != 0 {
		t.Errorf("refused send ran: %+v", calls)
	}
}

// TestSweep_FailedDeleteLoggedAndNotRedelivered: a delete that fails after
// the send ran is logged at ERROR, the send is not run again by a later
// sweep, and the delete is retried until it succeeds.
func TestSweep_FailedDeleteLoggedAndNotRedelivered(t *testing.T) {
	t.Parallel()
	logs := captureLog(t, log.DEBUG)
	d, mock := httpTestSetup(t, httpTestOpts{})
	path := filepath.Join(t.TempDir(), "def.db")
	store, err := defersend.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	d.deferStore = store
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Exec(`CREATE TRIGGER no_delete BEFORE DELETE ON deferred_sends BEGIN SELECT RAISE(ABORT, 'delete blocked by test'); END`); err != nil {
		t.Fatal(err)
	}

	sk := testAgentID + "/i" + runToken("defer3063")
	mock.entered = make(chan string, 4)
	enqueueReadySend(t, store, sk, "RUN ONCE DESPITE DELETE FAILURE")
	sw := sweepFor(d, store)
	sw.sweep()
	select {
	case <-mock.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("send never ran")
	}
	if !eventually(func() bool {
		return len(logs.Lines("session="+sk+")", "delete failed")) > 0
	}) {
		t.Fatalf("failed delete not logged:\n%s", logs.String())
	}
	if l := logs.Lines("session="+sk+")", "delete failed")[0]; l.Level != log.ERROR {
		t.Errorf("failed delete logged at %v, want ERROR: %s", l.Level, l.String())
	}
	sw.sweep()
	if n := len(mock.snapshot()); n != 1 {
		t.Fatalf("backend called %d times, want 1 (re-run after a failed delete)", n)
	}
	if l := logs.Lines("session="+sk+")", "dropping"); len(l) > 0 {
		t.Errorf("a record that ran was treated as dropped after its delete failed: %s", l[0].String())
	}
	if _, err := raw.Exec(`DROP TRIGGER no_delete`); err != nil {
		t.Fatal(err)
	}
	sw.sweep()
	if rows := storeRows(t, store); len(rows) != 0 {
		t.Errorf("delete not retried: %+v", rows)
	}
	if n := len(mock.snapshot()); n != 1 {
		t.Errorf("backend called %d times after the retried delete, want 1", n)
	}
}
