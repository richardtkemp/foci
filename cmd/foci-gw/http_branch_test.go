package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"foci/internal/defersend"
)

// TestWake_RunsWithWakeTrigger proves the /branch happy path with the gate open:
// 200, the response body carries the backend's reply, and the turn executes
// with the "branch" trigger label (which downstream code uses to distinguish
// branch turns from user turns).
func TestWake_RunsWithWakeTrigger(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mux := newTestMux(d)

	w := postJSON(mux, "/branch", `{"text":"morning check"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] != mockReply {
		t.Errorf("response = %q, want %q", resp["response"], mockReply)
	}
	calls := mock.snapshot()
	if len(calls) != 1 {
		t.Fatalf("backend calls = %d, want 1", len(calls))
	}
	if calls[0].trigger != "branch" {
		t.Errorf("trigger = %q, want branch", calls[0].trigger)
	}
	if !strings.Contains(calls[0].text, "morning check") {
		t.Errorf("backend saw %q, want the wake text", calls[0].text)
	}
}

// TestWake_GateClosedSkips proves /branch honours the activity gate: with recent
// last_activity on the target session and if_inactive set, the wake is skipped
// (200 with the canned skip response), no branch turn runs, and the agent
// backend is never touched. The gate matrix is unit-tested elsewhere; this
// pins that the handler actually consults it.
func TestWake_GateClosedSkips(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	d.sessionIndex.TouchCacheTouch(testSessionKey, time.Now())
	mux := newTestMux(d)

	w := postJSON(mux, "/branch", `{"text":"keepalive","if_inactive":"1h"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] != "skipped: session recently active" {
		t.Errorf("response = %q, want skip message", resp["response"])
	}
	if calls := mock.snapshot(); len(calls) != 0 {
		t.Errorf("backend called %d time(s) despite the gate skipping, want 0", len(calls))
	}
}

// TestWake_BranchFlow proves a non-delegated /branch runs its turn on a fresh
// branch of the parent session, not on the parent itself: the receipt reports
// a branch key (resolved_via "branch"), the turn's messages land in the branch
// session file, and the parent session file is untouched.
func TestWake_BranchFlow(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{})
	mux := newTestMux(d)

	w := postJSON(mux, "/branch", `{"text":"branch work"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)

	branchKey := resp["session"]
	if !strings.HasPrefix(branchKey, testSessionKey+"/b") {
		t.Fatalf("session = %q, want a branch of %s", branchKey, testSessionKey)
	}
	if resp["resolved_via"] != "branch" {
		t.Errorf("resolved_via = %q, want branch", resp["resolved_via"])
	}

	// The turn ran on the branch: its session file holds the exchange.
	branchMsgs, err := d.sessions.Load(branchKey)
	if err != nil {
		t.Fatalf("load branch session: %v", err)
	}
	if len(branchMsgs) == 0 {
		t.Error("branch session has no messages — the wake turn did not run on the branch")
	}

	// The parent stays clean: wake context is isolated to the branch.
	parentMsgs, err := d.sessions.Load(testSessionKey)
	if err != nil {
		t.Fatalf("load parent session: %v", err)
	}
	if len(parentMsgs) != 0 {
		t.Errorf("parent session gained %d message(s) from the wake turn, want 0", len(parentMsgs))
	}
}

// TestBranch_UnmetWaitDefers proves /branch honours the wait gates (#1272): a
// wait condition that does not hold NOW (evaluated against the resolved
// PARENT session) stores the request in the deferred queue instead of
// branching — HTTP 202 with the deferred receipt, no branch session created,
// no backend call — for async AND sync callers alike (a deferred request is
// inherently async; the caller's connection cannot be held until the
// condition holds).
func TestBranch_UnmetWaitDefers(t *testing.T) {
	warmSetup := func(t *testing.T) (httpHandlerDeps, *mockClient, *defersend.Store, *http.ServeMux) {
		d, mock := httpTestSetup(t, httpTestOpts{})
		store := withDeferStore(t, &d)
		d.sessionIndex.TouchCacheTouch(testSessionKey, time.Now()) // warm → wait_cold unmet
		return d, mock, store, newTestMux(d)
	}

	t.Run("async defers", func(t *testing.T) {
		_, mock, store, mux := warmSetup(t)
		w := postJSON(mux, "/branch", `{"text":"later","wait_cold":"1h","async":true}`)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code=%d want 202; body=%s", w.Code, w.Body.String())
		}
		var resp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["status"] != "deferred" {
			t.Errorf("status=%v want deferred; body=%s", resp["status"], w.Body.String())
		}
		if resp["session"] != testSessionKey {
			t.Errorf("session=%v want the parent %q", resp["session"], testSessionKey)
		}
		if all, _ := store.All(); len(all) != 1 {
			t.Errorf("queued=%d want 1", len(all))
		}
		if calls := mock.snapshot(); len(calls) != 0 {
			t.Errorf("backend called %d time(s) for a deferred branch, want 0", len(calls))
		}
	})

	t.Run("sync caller gets the same 202", func(t *testing.T) {
		_, mock, store, mux := warmSetup(t)
		w := postJSON(mux, "/branch", `{"text":"later","wait_cold":"1h","async":false}`)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code=%d want 202 (a deferred request is always async); body=%s", w.Code, w.Body.String())
		}
		var resp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["status"] != "deferred" {
			t.Errorf("status=%v want deferred; body=%s", resp["status"], w.Body.String())
		}
		if all, _ := store.All(); len(all) != 1 {
			t.Errorf("queued=%d want 1", len(all))
		}
		if calls := mock.snapshot(); len(calls) != 0 {
			t.Errorf("backend called %d time(s) for a deferred branch, want 0", len(calls))
		}
	})

	t.Run("record", func(t *testing.T) {
		_, _, store, mux := warmSetup(t)
		w := postJSON(mux, "/branch", `{"text":"morning check","wait_cold":"55m","wait_timeout":"3h","no_compact":true,"no_reset_hook":true,"silent":true,"model":"cheap"}`)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code=%d want 202; body=%s", w.Code, w.Body.String())
		}
		all, _ := store.All()
		if len(all) != 1 {
			t.Fatalf("queued=%d want 1", len(all))
		}
		r := all[0]
		// Model is stored VERBATIM, unvalidated: it is resolved at delivery
		// time (a bad model fails then, loudly — see
		// TestSweep_DropsBranchOnBadModel), so "cheap" — which the immediate
		// path would reject — defers fine.
		if r.Kind != defersend.KindBranch {
			t.Errorf("Kind=%q want %q", r.Kind, defersend.KindBranch)
		}
		if r.SessionKey != testSessionKey {
			t.Errorf("SessionKey=%q want the parent %q", r.SessionKey, testSessionKey)
		}
		if r.Text != "morning check" || !r.NoCompact || !r.NoResetHook || !r.Silent || r.Model != "cheap" {
			t.Errorf("branch fields not carried into the record: %+v", r)
		}
		if r.WaitCold != "55m" {
			t.Errorf("WaitCold=%q want 55m", r.WaitCold)
		}
	})

	t.Run("no branch session created", func(t *testing.T) {
		d, mock, _, mux := warmSetup(t)
		w := postJSON(mux, "/branch", `{"text":"later","wait_cold":"1h"}`)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code=%d want 202; body=%s", w.Code, w.Body.String())
		}
		entries, err := d.sessions.ScanAllSessions()
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.SessionKey, testSessionKey+"/b") {
				t.Errorf("branch session %q created for a deferred branch — the fork must happen at delivery time", e.SessionKey)
			}
		}
		if calls := mock.snapshot(); len(calls) != 0 {
			t.Errorf("backend called %d time(s), want 0", len(calls))
		}
	})
}

// TestBranch_WaitHoldsRunsNow is the holds-now half of #1272's branch wait
// gates, and pins that /branch gets NO default gate: a request with no wait
// field, or one whose condition already holds (or wait_none), branches
// immediately exactly as before — nothing is queued.
func TestBranch_WaitHoldsRunsNow(t *testing.T) {
	cases := []struct {
		name string
		warm bool
		body string
	}{
		{"no wait field runs now (no default gate)", false, `{"text":"now"}`},
		{"met condition runs now", false, `{"text":"now","wait_cold":"1m"}`},
		{"wait_none runs now even when unmet", true, `{"text":"now","wait_cold":"1h","wait_none":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, mock := httpTestSetup(t, httpTestOpts{})
			store := withDeferStore(t, &d)
			if tc.warm {
				d.sessionIndex.TouchCacheTouch(testSessionKey, time.Now())
			}
			mux := newTestMux(d)

			w := postJSON(mux, "/branch", tc.body)

			if w.Code != http.StatusOK {
				t.Fatalf("code=%d want 200 (runs now); body=%s", w.Code, w.Body.String())
			}
			if all, _ := store.All(); len(all) != 0 {
				t.Errorf("queued=%d want 0", len(all))
			}
			var resp map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if resp["response"] != mockReply {
				t.Errorf("response=%q want %q", resp["response"], mockReply)
			}
			if calls := mock.snapshot(); len(calls) != 1 || calls[0].trigger != "branch" {
				t.Errorf("calls=%+v want one branch turn", calls)
			}
		})
	}
}

// TestBranch_BadWaitDuration proves a malformed wait duration on /branch is a
// client error, not a deferred branch.
func TestBranch_BadWaitDuration(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{})
	withDeferStore(t, &d)
	mux := newTestMux(d)

	w := postJSON(mux, "/branch", `{"text":"x","wait_cold":"nope"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400; body=%s", w.Code, w.Body.String())
	}
}
