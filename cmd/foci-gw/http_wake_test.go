package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestWake_RunsWithWakeTrigger proves the /wake happy path with the gate open:
// 200, the response body carries the backend's reply, and the turn executes
// with the "wake" trigger label (which downstream code uses to distinguish
// wake turns from user turns).
func TestWake_RunsWithWakeTrigger(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mux := newTestMux(d)

	w := postJSON(mux, "/wake", `{"text":"morning check"}`)

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
	if calls[0].trigger != "wake" {
		t.Errorf("trigger = %q, want wake", calls[0].trigger)
	}
	if !strings.Contains(calls[0].text, "morning check") {
		t.Errorf("backend saw %q, want the wake text", calls[0].text)
	}
}

// TestWake_GateClosedSkips proves /wake honours the activity gate: with recent
// last_activity on the target session and if_inactive set, the wake is skipped
// (200 with the canned skip response), no branch turn runs, and the agent
// backend is never touched. The gate matrix is unit-tested elsewhere; this
// pins that the handler actually consults it.
func TestWake_GateClosedSkips(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	d.sessionIndex.SetSessionMetadata(testSessionKey, "last_activity", fmt.Sprintf("%d", time.Now().Unix()))
	mux := newTestMux(d)

	w := postJSON(mux, "/wake", `{"text":"keepalive","if_inactive":"1h"}`)

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

// TestWake_BranchFlow proves a non-delegated /wake runs its turn on a fresh
// branch of the parent session, not on the parent itself: the receipt reports
// a branch key (resolved_via "branch"), the turn's messages land in the branch
// session file, and the parent session file is untouched.
func TestWake_BranchFlow(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{})
	mux := newTestMux(d)

	w := postJSON(mux, "/wake", `{"text":"branch work"}`)

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
