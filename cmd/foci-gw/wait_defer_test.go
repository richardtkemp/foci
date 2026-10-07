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
	"foci/internal/ratelimit"
	"foci/internal/timeutil"
)

// engageRateLimit closes the test agent's endpoint rate-limit gate for
// roughly retryAfter (a trustworthy RetryAfter hint resolves to exactly
// now+retryAfter — see internal/ratelimit.Resolve).
func engageRateLimit(t *testing.T, d httpHandlerDeps, retryAfter time.Duration) {
	t.Helper()
	d.agents[testAgentID].ag.EngageRateLimit(ratelimit.Signal{Kind: ratelimit.KindRequest, RetryAfter: retryAfter})
}

func TestWaitSatisfied(t *testing.T) {
	active := func(_ string, _ time.Duration) bool { return true }
	inactive := func(_ string, _ time.Duration) bool { return false }
	in := activityGateInputs{SessionBase: "a/c1"}

	tests := []struct {
		name   string
		wc     waitConds
		isUser userActivityChecker
		isSess sessionActivityChecker
		want   bool
	}{
		{"warm holds when warm", waitConds{warm: "1m"}, active, active, true},
		{"warm unmet when cold", waitConds{warm: "1m"}, active, inactive, false},
		{"cold holds when cold", waitConds{cold: "1m"}, active, inactive, true},
		{"cold unmet when warm", waitConds{cold: "1m"}, active, active, false},
		{"user-active holds", waitConds{userActive: "1m"}, active, inactive, true},
		{"user-active unmet", waitConds{userActive: "1m"}, inactive, inactive, false},
		{"user-inactive holds", waitConds{userInactive: "1m"}, inactive, active, true},
		{"none set is satisfied", waitConds{}, active, active, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := waitSatisfied(tc.wc, in, tc.isUser, tc.isSess)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestWaitSatisfied_InFlightCountsActive(t *testing.T) {
	never := func(_ string, _ time.Duration) bool { return false }
	// A turn in flight makes the session "warm" even though the checker says cold,
	// so --wait-cold must NOT be satisfied while a turn runs.
	ok, err := waitSatisfied(waitConds{cold: "1m"}, activityGateInputs{InFlight: true}, never, never)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("wait-cold should be unsatisfied while a turn is in flight")
	}
}

func TestWaitSatisfied_RecentTurnEndCountsActive(t *testing.T) {
	never := func(_ string, _ time.Duration) bool { return false }
	// A turn that ENDED within the window keeps the session "active" even though
	// nothing is in flight now and the durable checker says cold — so --wait-cold
	// is NOT satisfied in the sub-window gap between back-to-back turns. This is
	// the send-8 mid-turn-release fix: without LastTurnEnd, a turn that started
	// >window ago reads cold the instant inFlight drops.
	in := activityGateInputs{LastTurnEnd: time.Now().Add(-10 * time.Second)}
	ok, err := waitSatisfied(waitConds{cold: "1m"}, in, never, never)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("wait-cold should be unsatisfied within the window after a turn ended")
	}

	// Once the dead time exceeds the window, cold holds (continuous silence).
	in.LastTurnEnd = time.Now().Add(-2 * time.Minute)
	ok, err = waitSatisfied(waitConds{cold: "1m"}, in, never, never)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("wait-cold should be satisfied once dead time exceeds the window")
	}
}

func TestWaitSatisfied_TurnEndDoesNotAffectUserGate(t *testing.T) {
	never := func(_ string, _ time.Duration) bool { return false }
	// A recent turn end is SESSION activity, not USER activity (it may be a
	// cron/agent/memory turn). --wait-user-inactive must still hold despite a
	// turn having just ended — LastTurnEnd feeds only the session probe.
	in := activityGateInputs{LastTurnEnd: time.Now().Add(-1 * time.Second)}
	ok, err := waitSatisfied(waitConds{userInactive: "1m"}, in, never, never)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("wait-user-inactive should hold: a turn ending is not user activity")
	}
}

func TestWaitSatisfied_BadDuration(t *testing.T) {
	if _, err := waitSatisfied(waitConds{cold: "nope"}, activityGateInputs{}, nil, nil); err == nil {
		t.Fatal("expected error for malformed duration")
	}
}

func withDeferStore(t *testing.T, d *httpHandlerDeps) *defersend.Store {
	t.Helper()
	s, err := defersend.NewStore(filepath.Join(t.TempDir(), "def.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	d.deferStore = s
	return s
}

func TestSend_DefaultDefersWhenWarm(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{})
	store := withDeferStore(t, &d)
	d.sessionIndex.TouchCacheTouch(testSessionKey, time.Now()) // warm → default wait_cold=1m unmet
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"later"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d want 202; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "deferred" {
		t.Errorf("status=%v want deferred", resp["status"])
	}
	if all, _ := store.All(); len(all) != 1 {
		t.Errorf("queued=%d want 1", len(all))
	}
}

func TestSend_WaitNoneBypasses(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{})
	store := withDeferStore(t, &d)
	d.sessionIndex.TouchCacheTouch(testSessionKey, time.Now()) // warm — default would otherwise defer
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"now","wait_none":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d want 200; body=%s", w.Code, w.Body.String())
	}
	if all, _ := store.All(); len(all) != 0 {
		t.Errorf("queued=%d want 0 (wait_none must not defer)", len(all))
	}
}

func TestSend_ExplicitWaitDefers(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{})
	store := withDeferStore(t, &d)
	mux := newTestMux(d)

	// Session is cold; --wait-warm requires warmth → unmet → defer.
	w := postJSON(mux, "/send", `{"text":"x","wait_warm":"1h"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d want 202; body=%s", w.Code, w.Body.String())
	}
	if all, _ := store.All(); len(all) != 1 {
		t.Errorf("queued=%d want 1", len(all))
	}
}

// TestSend_DefersWhenRateLimited is the red test for #1417: an incoming
// /send whose target endpoint is currently rate-limited must queue rather
// than dispatch (which would be a guaranteed-fail API call, dropped silently
// for an async send). Unlike the activity wait gates, this is a hard
// capacity constraint, not a scheduling preference — it must defer even
// under wait_none ("--no-gate"), which would otherwise send immediately.
func TestSend_DefersWhenRateLimited(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	store := withDeferStore(t, &d)
	engageRateLimit(t, d, time.Hour)
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"now please","wait_none":true}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d want 202; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "deferred" {
		t.Errorf("status=%v want deferred", resp["status"])
	}
	if all, _ := store.All(); len(all) != 1 {
		t.Errorf("queued=%d want 1", len(all))
	}
	if calls := mock.snapshot(); len(calls) != 0 {
		t.Errorf("backend called %d time(s) while rate limited, want 0: %+v", len(calls), calls)
	}
}

// TestSweep_WithholdsWhileRateLimited proves the sweep side of #1417: a
// pending deferred send whose activity conditions are already satisfied must
// still be withheld while the endpoint's rate-limit gate is closed, and
// delivered exactly once the gate reopens — no "send anyway on deadline"
// escape hatch for rate limiting (that would just fail again).
func TestSweep_WithholdsWhileRateLimited(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mock.entered = make(chan string, 1)
	store := withDeferStore(t, &d)
	now := timeutil.Now()
	_, _ = store.Enqueue(defersend.Record{
		AgentID: testAgentID, SessionKey: testSessionKey, Text: "held msg", Policy: "fallback",
		CreatedAt: now, DeadlineAt: now.Add(time.Hour),
	})
	engageRateLimit(t, d, 150*time.Millisecond)
	isU, isS := buildActivityCheckers(d)
	sw := &deferSweeper{store: store, deps: d, isUserActive: isU, isSessionActive: isS}

	sw.sweep()
	select {
	case text := <-mock.entered:
		t.Fatalf("delivered %q while the endpoint was rate limited", text)
	case <-time.After(200 * time.Millisecond):
	}
	if all, _ := store.All(); len(all) != 1 {
		t.Errorf("queued=%d want 1 (still withheld)", len(all))
	}

	time.Sleep(200 * time.Millisecond) // past the 150ms gate deadline
	sw.sweep()
	select {
	case text := <-mock.entered:
		if !strings.HasSuffix(text, "held msg") {
			t.Errorf("delivered text = %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("send was not delivered once the rate limit cleared")
	}
	if all, _ := store.All(); len(all) != 0 {
		t.Errorf("store not drained: %d", len(all))
	}
}

func TestSweep_DeliversWhenSatisfied(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mock.entered = make(chan string, 1)
	store := withDeferStore(t, &d)
	now := timeutil.Now()
	// Cold session → wait_cold satisfied on the next sweep.
	_, _ = store.Enqueue(defersend.Record{
		AgentID: testAgentID, SessionKey: testSessionKey, Text: "queued msg", Policy: "fallback",
		WaitCold: "1m", CreatedAt: now, DeadlineAt: now.Add(time.Hour),
	})
	isU, isS := buildActivityCheckers(d)
	sw := &deferSweeper{store: store, deps: d, isUserActive: isU, isSessionActive: isS}
	sw.sweep()

	select {
	case text := <-mock.entered:
		if !strings.HasSuffix(text, "queued msg") {
			t.Errorf("delivered text = %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("satisfied send was not delivered by the sweep")
	}
	if all, _ := store.All(); len(all) != 0 {
		t.Errorf("store not drained: %d", len(all))
	}
}

func TestSweep_DeliversOnDeadline(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mock.entered = make(chan string, 1)
	store := withDeferStore(t, &d)
	now := timeutil.Now()
	// wait_warm on a cold session never holds, but the deadline has passed → send anyway.
	_, _ = store.Enqueue(defersend.Record{
		AgentID: testAgentID, SessionKey: testSessionKey, Text: "deadline msg", Policy: "fallback",
		WaitWarm: "1h", CreatedAt: now.Add(-3 * time.Hour), DeadlineAt: now.Add(-time.Hour),
	})
	isU, isS := buildActivityCheckers(d)
	sw := &deferSweeper{store: store, deps: d, isUserActive: isU, isSessionActive: isS}
	sw.sweep()

	select {
	case <-mock.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("deadline-expired send was not delivered")
	}
	if all, _ := store.All(); len(all) != 0 {
		t.Errorf("store not drained: %d", len(all))
	}
}

// sweepFor builds a sweeper around the harness deps, the way main.go does.
func sweepFor(d httpHandlerDeps, store *defersend.Store) *deferSweeper {
	isU, isS := buildActivityCheckers(d)
	return &deferSweeper{store: store, deps: d, isUserActive: isU, isSessionActive: isS}
}

// eventually polls cond until it holds or the deadline passes, returning
// whether it ever held. The sweep's branch delivery is async (the turn runs on
// the session's inbox worker), so the observable (backend call, session file)
// lags the sweep() call.
func eventually(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// TestSweep_DeliversBranchRecord proves a deferred branch record is delivered
// as a BRANCH: the fork happens at delivery time from the stored parent, the
// turn runs async with trigger "branch" on the new branch session, the parent
// session stays untouched, no receipt is expected (the caller is long gone),
// and the record is deleted after delivery.
func TestSweep_DeliversBranchRecord(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mock.entered = make(chan string, 1)
	store := withDeferStore(t, &d)
	now := timeutil.Now()
	// wait_cold holds (session never touched) → deliverable on the first sweep.
	_, _ = store.Enqueue(defersend.Record{
		Kind: defersend.KindBranch, AgentID: testAgentID, SessionKey: testSessionKey,
		Text: "deferred branch work", Model: "", NoCompact: true, NoResetHook: true, Silent: true,
		WaitCold: "1m", CreatedAt: now, DeadlineAt: now.Add(time.Hour),
	})
	sweepFor(d, store).sweep()

	select {
	case <-mock.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("deferred branch was not delivered by the sweep")
	}
	calls := mock.snapshot()
	if len(calls) != 1 || calls[0].trigger != "branch" {
		t.Fatalf("calls=%+v want exactly one turn with trigger %q", calls, "branch")
	}
	if !strings.Contains(calls[0].text, "deferred branch work") {
		t.Errorf("backend saw %q, want the branch text", calls[0].text)
	}

	// The turn ran on a NEW branch of the parent, and the parent stays clean.
	var branchKey string
	if !eventually(func() bool {
		entries, err := d.sessions.ScanAllSessions()
		if err != nil {
			return false
		}
		for _, e := range entries {
			if strings.HasPrefix(e.SessionKey, testSessionKey+"/b") {
				branchKey = e.SessionKey
				return true
			}
		}
		return false
	}) {
		t.Fatal("no branch session was created by the sweep delivery")
	}
	if !eventually(func() bool {
		msgs, err := d.sessions.Load(branchKey)
		return err == nil && len(msgs) > 0
	}) {
		t.Error("branch session has no messages — the turn did not run on the branch")
	}
	parentMsgs, err := d.sessions.Load(testSessionKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(parentMsgs) != 0 {
		t.Errorf("parent session gained %d message(s), want 0", len(parentMsgs))
	}
	if all, _ := store.All(); len(all) != 0 {
		t.Errorf("store not drained: %d", len(all))
	}
}

// TestSweep_DeliversCommandRecord proves a deferred command record is
// dispatched through the agent's command registry against the stored session
// and deleted after the one delivery.
func TestSweep_DeliversCommandRecord(t *testing.T) {
	spy, runs := resetSpy()
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{spy}})
	store := withDeferStore(t, &d)
	now := timeutil.Now()
	_, _ = store.Enqueue(defersend.Record{
		Kind: defersend.KindCommand, AgentID: testAgentID, SessionKey: testSessionKey,
		Text: "/reset", WaitCold: "1m", CreatedAt: now, DeadlineAt: now.Add(time.Hour),
	})

	sweepFor(d, store).sweep()

	if n := runs.Load(); n != 1 {
		t.Errorf("command executed %d time(s), want 1", n)
	}
	if all, _ := store.All(); len(all) != 0 {
		t.Errorf("store not drained: %d", len(all))
	}
}

// TestSweep_DeliversBranchOnDeadline proves the send-anyway-on-deadline rule
// covers branches: a wait that never held delivers the branch once the
// deadline has passed.
func TestSweep_DeliversBranchOnDeadline(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mock.entered = make(chan string, 1)
	store := withDeferStore(t, &d)
	now := timeutil.Now()
	// wait_warm on a never-touched session never holds; the deadline passed → branch anyway.
	_, _ = store.Enqueue(defersend.Record{
		Kind: defersend.KindBranch, AgentID: testAgentID, SessionKey: testSessionKey,
		Text: "deadline branch", WaitWarm: "1h",
		CreatedAt: now.Add(-3 * time.Hour), DeadlineAt: now.Add(-time.Hour),
	})
	sweepFor(d, store).sweep()

	select {
	case <-mock.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("deadline-expired branch was not delivered")
	}
	if calls := mock.snapshot(); len(calls) != 1 || calls[0].trigger != "branch" {
		t.Errorf("calls=%+v want one branch turn", calls)
	}
	if all, _ := store.All(); len(all) != 0 {
		t.Errorf("store not drained: %d", len(all))
	}
}

// TestSweep_WithholdsBranchWhileRateLimited mirrors
// TestSweep_WithholdsWhileRateLimited for branches (#1272 req 7): the sweep's
// rate-limit hold applies to every kind, with no send-anyway escape hatch —
// the branch waits while its parent's endpoint gate is closed and is
// delivered once the gate reopens.
func TestSweep_WithholdsBranchWhileRateLimited(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mock.entered = make(chan string, 1)
	store := withDeferStore(t, &d)
	now := timeutil.Now()
	_, _ = store.Enqueue(defersend.Record{
		Kind: defersend.KindBranch, AgentID: testAgentID, SessionKey: testSessionKey,
		Text: "held branch", WaitCold: "1m", CreatedAt: now, DeadlineAt: now.Add(time.Hour),
	})
	engageRateLimit(t, d, 150*time.Millisecond)
	sw := sweepFor(d, store)

	sw.sweep()
	select {
	case text := <-mock.entered:
		t.Fatalf("delivered branch %q while the endpoint was rate limited", text)
	case <-time.After(200 * time.Millisecond):
	}
	if all, _ := store.All(); len(all) != 1 {
		t.Errorf("queued=%d want 1 (still withheld)", len(all))
	}

	time.Sleep(200 * time.Millisecond) // past the 150ms gate deadline
	sw.sweep()
	select {
	case <-mock.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("branch was not delivered once the rate limit cleared")
	}
	if all, _ := store.All(); len(all) != 0 {
		t.Errorf("store not drained: %d", len(all))
	}
}
