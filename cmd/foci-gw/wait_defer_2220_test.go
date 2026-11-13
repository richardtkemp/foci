package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/defersend"
	"foci/internal/sqlite"
	"foci/internal/timeutil"
)

// #2220 — deferred (--wait-*) requests: validate at enqueue, retry then
// tell, dedupe, drop commands at the deadline, 24 h default deadline.

// noBranchSession reports whether no branch of testSessionKey exists.
func noBranchSession(t *testing.T, d httpHandlerDeps) bool {
	t.Helper()
	entries, err := d.sessions.ScanAllSessions()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.SessionKey, testSessionKey+"/b") {
			return false
		}
	}
	return true
}

func storedCount(t *testing.T, s *defersend.Store) int {
	t.Helper()
	all, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	return len(all)
}

// --- Requirement 1: validate at enqueue ---

func TestDeferEnqueue_UnknownCommandRefused(t *testing.T) {
	t.Parallel()
	d, _ := httpTestSetup(t, httpTestOpts{})
	store := withDeferStore(t, &d)
	mux := newTestMux(d)

	// wait_warm on a never-touched session is unmet → would defer.
	w := postJSON(mux, "/command", `{"command":"/rset","wait_warm":"1h"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404; body=%s", w.Code, w.Body.String())
	}
	if got := strings.TrimSpace(w.Body.String()); got != "unknown command" {
		t.Errorf("body=%q want %q", got, "unknown command")
	}
	if n := storedCount(t, store); n != 0 {
		t.Errorf("stored=%d want 0", n)
	}
}

func TestDeferEnqueue_BranchBadModelRefused(t *testing.T) {
	t.Parallel()
	d, mock := httpTestSetup(t, httpTestOpts{})
	store := withDeferStore(t, &d)
	mux := newTestMux(d)

	w := postJSON(mux, "/branch", `{"text":"x","model":"cheap","wait_warm":"1h"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400; body=%s", w.Code, w.Body.String())
	}
	// The immediate path's text for the same model (errBranchBadModel).
	imm := postJSON(mux, "/branch", `{"text":"x","model":"cheap"}`)
	if imm.Code != http.StatusBadRequest || w.Body.String() != imm.Body.String() {
		t.Errorf("enqueue refusal %d %q, immediate %d %q: want the same", w.Code, w.Body.String(), imm.Code, imm.Body.String())
	}
	if !strings.HasPrefix(w.Body.String(), "bad model: ") {
		t.Errorf("body=%q want the errBranchBadModel text", w.Body.String())
	}
	if n := storedCount(t, store); n != 0 {
		t.Errorf("stored=%d want 0", n)
	}
	if calls := mock.snapshot(); len(calls) != 0 {
		t.Errorf("backend called %d time(s), want 0", len(calls))
	}
}

// TestDeferEnqueue_BranchBadModel_NoBranchSession checks the enqueue refusal
// alone creates no branch session (the immediate path creates one before it
// applies the model, so it is not exercised here).
func TestDeferEnqueue_BranchBadModel_NoBranchSession(t *testing.T) {
	t.Parallel()
	d, _ := httpTestSetup(t, httpTestOpts{})
	withDeferStore(t, &d)
	mux := newTestMux(d)

	w := postJSON(mux, "/branch", `{"text":"x","model":"cheap","wait_warm":"1h"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400; body=%s", w.Code, w.Body.String())
	}
	if !noBranchSession(t, d) {
		t.Error("a branch session was created for a refused deferred branch")
	}
}

func TestDeferEnqueue_SendBadModelRefused(t *testing.T) {
	t.Parallel()
	d, _ := httpTestSetup(t, httpTestOpts{})
	store := withDeferStore(t, &d)
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"x","model":"cheap","wait_warm":"1h"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400; body=%s", w.Code, w.Body.String())
	}
	imm := postJSON(mux, "/send", `{"text":"x","model":"cheap","wait_none":true}`)
	if imm.Code != http.StatusBadRequest || w.Body.String() != imm.Body.String() {
		t.Errorf("enqueue refusal %d %q, immediate %d %q: want the same", w.Code, w.Body.String(), imm.Code, imm.Body.String())
	}
	if n := storedCount(t, store); n != 0 {
		t.Errorf("stored=%d want 0", n)
	}
}

// Characterisation: a request with no wait is unchanged — an unknown
// command still answers 200 with the registry's suggestion, a bad model on
// an immediate branch/send still answers 400, and nothing is stored.
func TestDeferEnqueue_NoWaitUnchanged(t *testing.T) {
	t.Parallel()
	d, _ := httpTestSetup(t, httpTestOpts{})
	store := withDeferStore(t, &d)
	mux := newTestMux(d)

	if w := postJSON(mux, "/command", `{"command":"/rset"}`); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Unknown command") {
		t.Errorf("/command no wait: %d %s", w.Code, w.Body.String())
	}
	if w := postJSON(mux, "/branch", `{"text":"x","model":"cheap"}`); w.Code != http.StatusBadRequest {
		t.Errorf("/branch no wait: %d %s", w.Code, w.Body.String())
	}
	if w := postJSON(mux, "/send", `{"text":"x","model":"cheap","wait_none":true}`); w.Code != http.StatusBadRequest {
		t.Errorf("/send no wait: %d %s", w.Code, w.Body.String())
	}
	if n := storedCount(t, store); n != 0 {
		t.Errorf("stored=%d want 0", n)
	}
}

// Characterisation: a valid deferred command is still stored (202).
func TestDeferEnqueue_KnownCommandStillDefers(t *testing.T) {
	t.Parallel()
	spy, _ := resetSpy()
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{spy}})
	store := withDeferStore(t, &d)
	mux := newTestMux(d)

	if w := postJSON(mux, "/command", `{"command":"/reset","wait_warm":"1h"}`); w.Code != http.StatusAccepted {
		t.Fatalf("code=%d want 202; body=%s", w.Code, w.Body.String())
	}
	if n := storedCount(t, store); n != 1 {
		t.Errorf("stored=%d want 1", n)
	}
}

// --- Requirement 2: retry, then a drop notice ---

// noticeSweeper builds a sweeper whose target session has a recording
// connection, returning the connection.
func noticeSweeper(t *testing.T, d *httpHandlerDeps, store *defersend.Store) (*deferSweeper, *recordingConn) {
	t.Helper()
	conn := newRecordingConn(testSessionKey)
	d.connMgr = recordingConnMgr{sessionKey: testSessionKey, conn: conn}
	return sweepFor(*d, store), conn
}

// noticeTexts returns the drop notices conn received. A drop notice is foci's
// notice, so it must arrive through the injected (system-role) send, addressed
// to testSessionKey's chat, never as an agent-role SendToSession (#3005).
func noticeTexts(t *testing.T, c *recordingConn) []string {
	t.Helper()
	if _, agentRole := c.snapshot(); len(agentRole) != 0 {
		t.Errorf("notice sent with the agent role (SendToSession): %v", agentRole)
	}
	var texts []string
	for _, rec := range c.injectedSnapshot() {
		key, text, _ := strings.Cut(rec, "|")
		if key != testSessionKey {
			t.Errorf("notice addressed to session %q, want %q", key, testSessionKey)
		}
		texts = append(texts, text)
	}
	return texts
}

func TestSweep_TransientFailureRetriesThenDrops(t *testing.T) {
	t.Parallel()
	d, _ := httpTestSetup(t, httpTestOpts{})
	path := filepath.Join(t.TempDir(), "def.db")
	store, err := defersend.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := timeutil.Now()
	id, _, _ := store.Enqueue(defersend.Record{
		Kind: defersend.KindCommand, AgentID: testAgentID, SessionKey: testSessionKey,
		Text: "/reset hunter2", WaitCold: "1m", CreatedAt: now, DeadlineAt: now.Add(time.Hour),
	})
	sw, conn := noticeSweeper(t, &d, store)
	attempts := 0
	sw.deliverFn = func(*agentInstance, defersend.Record) error { attempts++; return errBranchInboxFull }

	sw.sweep()
	sw.sweep()
	if attempts != 2 {
		t.Fatalf("delivery attempts=%d want 2", attempts)
	}
	if n := storedCount(t, store); n != 1 {
		t.Fatalf("stored=%d after 2 transient failures, want 1 (kept for retry)", n)
	}
	if texts := noticeTexts(t, conn); len(texts) != 0 {
		t.Fatalf("notice sent before the third failure: %v", texts)
	}

	// The count survives a restart: reopen the store file.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = defersend.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sw.store = store
	if all, _ := store.All(); len(all) != 1 || all[0].Attempts != 2 {
		t.Fatalf("after reopen: %+v, want Attempts=2", all)
	}

	sw.sweep()
	if attempts != 3 {
		t.Fatalf("delivery attempts=%d want 3", attempts)
	}
	if n := storedCount(t, store); n != 0 {
		t.Fatalf("stored=%d after the third failure, want 0", n)
	}
	texts := noticeTexts(t, conn)
	if len(texts) != 1 {
		t.Fatalf("notices=%v want exactly one", texts)
	}
	assertDropNotice(t, texts[0], "command", id)

	sw.sweep()
	if attempts != 3 {
		t.Errorf("record delivered again after the drop (attempts=%d)", attempts)
	}
}

func assertDropNotice(t *testing.T, notice, kind string, id int64) {
	t.Helper()
	if !strings.Contains(notice, kind) || !strings.Contains(notice, "#"+itoa(id)) {
		t.Errorf("notice %q must name the kind %q and the id #%d", notice, kind, id)
	}
	for _, leak := range []string{"/reset", "hunter2", "reset hunter2"} {
		if strings.Contains(notice, leak) {
			t.Errorf("notice %q leaks the command text (%q)", notice, leak)
		}
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestSweep_PermanentFailureDropsAtOnce(t *testing.T) {
	t.Parallel()
	d, _ := httpTestSetup(t, httpTestOpts{})
	store := withDeferStore(t, &d)
	now := timeutil.Now()
	id, _, _ := store.Enqueue(defersend.Record{
		Kind: defersend.KindBranch, AgentID: testAgentID, SessionKey: testSessionKey,
		Text: "doomed", Model: "cheap", WaitCold: "1m", CreatedAt: now, DeadlineAt: now.Add(time.Hour),
	})
	sw, conn := noticeSweeper(t, &d, store)

	sw.sweep() // real delivery: the model does not resolve → permanent

	if n := storedCount(t, store); n != 0 {
		t.Fatalf("stored=%d want 0", n)
	}
	texts := noticeTexts(t, conn)
	if len(texts) != 1 {
		t.Fatalf("notices=%v want one", texts)
	}
	assertDropNotice(t, texts[0], "branch", id)
	if !strings.Contains(texts[0], "bad model") {
		t.Errorf("notice %q should carry the reason class", texts[0])
	}
}

// TestSweep_UnknownCommandAtDelivery_DroppedWithNotice: a record naming a
// command that is not registered (the registry changed during the wait) is
// a permanent failure, and its notice never includes the command text.
func TestSweep_UnknownCommandAtDelivery_DroppedWithNotice(t *testing.T) {
	t.Parallel()
	d, _ := httpTestSetup(t, httpTestOpts{})
	store := withDeferStore(t, &d)
	now := timeutil.Now()
	id, _, _ := store.Enqueue(defersend.Record{
		Kind: defersend.KindCommand, AgentID: testAgentID, SessionKey: testSessionKey,
		Text: "/reset hunter2", WaitCold: "1m", CreatedAt: now, DeadlineAt: now.Add(time.Hour),
	})
	sw, conn := noticeSweeper(t, &d, store)
	sw.sweep()

	if n := storedCount(t, store); n != 0 {
		t.Fatalf("stored=%d want 0", n)
	}
	texts := noticeTexts(t, conn)
	if len(texts) != 1 {
		t.Fatalf("notices=%v want one", texts)
	}
	assertDropNotice(t, texts[0], "command", id)
	if !strings.Contains(texts[0], "unknown command") {
		t.Errorf("notice %q should carry the reason class \"unknown command\"", texts[0])
	}
}

// The notice goes only to the target session's own chat: a connection for a
// different session (the agent's primary) gets nothing.
func TestSweep_DropNoticeOnlyToOwnSession(t *testing.T) {
	t.Parallel()
	d, _ := httpTestSetup(t, httpTestOpts{})
	store := withDeferStore(t, &d)
	now := timeutil.Now()
	_, _, _ = store.Enqueue(defersend.Record{
		Kind: defersend.KindBranch, AgentID: testAgentID, SessionKey: testSessionKey,
		Text: "doomed", Model: "cheap", WaitCold: "1m", CreatedAt: now, DeadlineAt: now.Add(time.Hour),
	})
	other := newRecordingConn("someone/else")
	d.connMgr = recordingConnMgr{sessionKey: "someone/else", conn: other}
	sweepFor(d, store).sweep()

	if n := storedCount(t, store); n != 0 {
		t.Fatalf("stored=%d want 0", n)
	}
	if texts := noticeTexts(t, other); len(texts) != 0 {
		t.Errorf("notice went to a connection that is not the session's own: %v", texts)
	}
}

// Characterisation: success removes the record and sends no notice.
func TestSweep_SuccessRemovesRecord_NoNotice(t *testing.T) {
	t.Parallel()
	spy, runs := resetSpy()
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{spy}})
	store := withDeferStore(t, &d)
	now := timeutil.Now()
	_, _, _ = store.Enqueue(defersend.Record{
		Kind: defersend.KindCommand, AgentID: testAgentID, SessionKey: testSessionKey,
		Text: "/reset", WaitCold: "1m", CreatedAt: now, DeadlineAt: now.Add(time.Hour),
	})
	sw, conn := noticeSweeper(t, &d, store)
	sw.sweep()
	if runs.Load() != 1 {
		t.Errorf("runs=%d want 1", runs.Load())
	}
	if n := storedCount(t, store); n != 0 {
		t.Errorf("stored=%d want 0", n)
	}
	if texts := noticeTexts(t, conn); len(texts) != 0 {
		t.Errorf("notice on success: %v", texts)
	}
}

// --- Requirement 2 / ticket item 4: an old-schema row is delivered end to end ---

// Characterisation (closes a coverage gap; old code delivered these too):
// a store file written with an EARLIER deferred_sends schema — the original
// pre-kind one and the one just before #2220 (no attempts) — opens and its
// row is delivered by a sweep, end to end.
func TestSweep_DeliversOldSchemaRow(t *testing.T) {
	t.Parallel()
	for name, ddl := range map[string]string{
		"pre-kind": `CREATE TABLE deferred_sends (
			id INTEGER PRIMARY KEY AUTOINCREMENT, agent_id TEXT NOT NULL, session_key TEXT NOT NULL,
			text TEXT NOT NULL, policy TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
			wait_warm TEXT NOT NULL DEFAULT '', wait_cold TEXT NOT NULL DEFAULT '',
			wait_user_active TEXT NOT NULL DEFAULT '', wait_user_inactive TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL, deadline_at TEXT NOT NULL)`,
		"pre-attempts": `CREATE TABLE deferred_sends (
			id INTEGER PRIMARY KEY AUTOINCREMENT, agent_id TEXT NOT NULL, session_key TEXT NOT NULL,
			text TEXT NOT NULL, policy TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
			steer TEXT NOT NULL DEFAULT '',
			wait_warm TEXT NOT NULL DEFAULT '', wait_cold TEXT NOT NULL DEFAULT '',
			wait_user_active TEXT NOT NULL DEFAULT '', wait_user_inactive TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL, deadline_at TEXT NOT NULL, kind TEXT NOT NULL DEFAULT '',
			no_compact INTEGER NOT NULL DEFAULT 0, no_reset_hook INTEGER NOT NULL DEFAULT 0,
			silent INTEGER NOT NULL DEFAULT 0)`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, mock := httpTestSetup(t, httpTestOpts{})
			mock.entered = make(chan string, 1)
			path := filepath.Join(t.TempDir(), "old.db")
			db, err := sqlite.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(ddl); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO deferred_sends
				(agent_id, session_key, text, policy, wait_cold, created_at, deadline_at)
				VALUES (?, ?, 'queued long ago', 'fallback', '1m', '2026-01-01T00:00:00Z', '2099-01-01T00:00:00Z')`,
				testAgentID, testSessionKey); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			store, err := defersend.NewStore(path)
			if err != nil {
				t.Fatalf("NewStore on %s db: %v", name, err)
			}
			t.Cleanup(func() { _ = store.Close() })

			sweepFor(d, store).sweep()
			select {
			case text := <-mock.entered:
				if !strings.HasSuffix(text, "queued long ago") {
					t.Errorf("delivered %q", text)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("old-schema row was not delivered")
			}
			// The row goes once the turn has run, after it entered (#3063).
			if !eventually(func() bool { return storedCount(t, store) == 0 }) {
				t.Errorf("stored=%d want 0", storedCount(t, store))
			}
		})
	}
}

// --- Requirement 3: dedupe ---

func TestDeferEnqueue_IdenticalRequestsDedupe(t *testing.T) {
	t.Parallel()
	spy, _ := resetSpy()
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{spy}})
	store := withDeferStore(t, &d)
	mux := newTestMux(d)

	body := `{"command":"/reset","wait_warm":"55m"}`
	var ids []float64
	for i := 0; i < 2; i++ {
		w := postJSON(mux, "/command", body)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code=%d want 202; body=%s", w.Code, w.Body.String())
		}
		var resp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		ids = append(ids, resp["deferred_id"].(float64))
	}
	all, err := store.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("stored=%d want 1 (identical requests deduplicated)", len(all))
	}
	if float64(all[0].ID) != ids[1] || ids[0] == ids[1] {
		t.Errorf("kept id %d, receipts %v: want the newer record kept and named by the second receipt", all[0].ID, ids)
	}

	// A differing condition is a different request.
	if w := postJSON(mux, "/command", `{"command":"/reset","wait_warm":"56m"}`); w.Code != http.StatusAccepted {
		t.Fatalf("code=%d", w.Code)
	}
	if n := storedCount(t, store); n != 2 {
		t.Errorf("stored=%d want 2 (different condition)", n)
	}
}

// --- Requirement 5: commands are dropped at the deadline ---

func TestSweep_CommandPastDeadlineDropped(t *testing.T) {
	t.Parallel()
	spy, runs := resetSpy()
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{spy}})
	store := withDeferStore(t, &d)
	now := timeutil.Now()
	// wait_warm on a never-touched session never holds; the deadline passed.
	id, _, _ := store.Enqueue(defersend.Record{
		Kind: defersend.KindCommand, AgentID: testAgentID, SessionKey: testSessionKey,
		Text: "/reset hunter2", WaitWarm: "1h", CreatedAt: now.Add(-25 * time.Hour), DeadlineAt: now.Add(-time.Hour),
	})
	sw, conn := noticeSweeper(t, &d, store)
	sw.sweep()

	if runs.Load() != 0 {
		t.Errorf("command dispatched %d time(s) at its deadline, want 0", runs.Load())
	}
	if n := storedCount(t, store); n != 0 {
		t.Errorf("stored=%d want 0", n)
	}
	texts := noticeTexts(t, conn)
	if len(texts) != 1 {
		t.Fatalf("notices=%v want one", texts)
	}
	assertDropNotice(t, texts[0], "command", id)
	if !strings.Contains(texts[0], "deadline") {
		t.Errorf("notice %q should say the deadline was reached", texts[0])
	}
}

// Characterisation: a command whose condition holds is delivered even when
// its deadline has also passed.
func TestSweep_CommandConditionHeldPastDeadlineDelivered(t *testing.T) {
	t.Parallel()
	spy, runs := resetSpy()
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{spy}})
	store := withDeferStore(t, &d)
	now := timeutil.Now()
	_, _, _ = store.Enqueue(defersend.Record{
		Kind: defersend.KindCommand, AgentID: testAgentID, SessionKey: testSessionKey,
		Text: "/reset", WaitCold: "1m", CreatedAt: now.Add(-25 * time.Hour), DeadlineAt: now.Add(-time.Hour),
	})
	sweepFor(d, store).sweep()
	if runs.Load() != 1 {
		t.Errorf("runs=%d want 1", runs.Load())
	}
}

// (Sends and branches past their deadline are still delivered:
// TestSweep_DeliversOnDeadline and TestSweep_DeliversBranchOnDeadline.)

// --- Requirement 6: default deadline 24 h ---

func TestDeferEnqueue_DefaultDeadline24h(t *testing.T) {
	t.Parallel()
	spy, _ := resetSpy()
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{spy}})
	store := withDeferStore(t, &d)
	mux := newTestMux(d)

	if w := postJSON(mux, "/command", `{"command":"/reset","wait_warm":"1h"}`); w.Code != http.StatusAccepted {
		t.Fatalf("code=%d", w.Code)
	}
	all, _ := store.All()
	if len(all) != 1 {
		t.Fatalf("stored=%d", len(all))
	}
	if got := all[0].DeadlineAt.Sub(all[0].CreatedAt); got != 24*time.Hour {
		t.Errorf("deadline - created = %v, want 24h", got)
	}
}

// Characterisation: an explicit wait_timeout still overrides the default.
func TestDeferEnqueue_ExplicitTimeoutOverrides(t *testing.T) {
	t.Parallel()
	spy, _ := resetSpy()
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{spy}})
	store := withDeferStore(t, &d)
	mux := newTestMux(d)

	if w := postJSON(mux, "/command", `{"command":"/reset","wait_warm":"1h","wait_timeout":"30m"}`); w.Code != http.StatusAccepted {
		t.Fatalf("code=%d", w.Code)
	}
	all, _ := store.All()
	if len(all) != 1 {
		t.Fatalf("stored=%d", len(all))
	}
	if got := all[0].DeadlineAt.Sub(all[0].CreatedAt); got != 30*time.Minute {
		t.Errorf("deadline - created = %v, want 30m", got)
	}
}
