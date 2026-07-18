package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/defersend"
	"foci/internal/timeutil"
)

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
