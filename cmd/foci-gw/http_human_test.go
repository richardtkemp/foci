package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"foci/internal/command"
)

// userActivityRecorded reports whether the session index has a
// last_user_activity_at for sessionKey — the durable "a human touched this
// session" signal the human-declaration tests (#1130) assert on.
func userActivityRecorded(d httpHandlerDeps, sessionKey string) bool {
	_, ok := d.sessionIndex.LastUserActivity(sessionKey)
	return ok
}

// TestSend_HumanSyncSetsUserActivity proves the #1130 send path: a
// synchronous /send that declares "human": true runs its turn with the
// human-source marker, so the turn's entry write stamps
// last_user_activity_at on the target session — a person typing
// `foci send --human` counts as user attention like a platform turn. The
// harness wires no periodic runner, so passing also proves the receipt stamp
// is nil-runner safe.
func TestSend_HumanSyncSetsUserActivity(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"hi","human":true,"wait_none":true}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if !userActivityRecorded(d, testSessionKey) {
		t.Error("human sync send did not set last_user_activity_at on the target session")
	}
	if calls := mock.snapshot(); len(calls) != 1 {
		t.Errorf("backend calls = %d, want 1", len(calls))
	}
}

// TestSend_AutomatedSyncNoUserActivity is the characterisation half of #1130
// req 2: without the human declaration a /send behaves exactly as before —
// automated, no user activity recorded on the target session.
func TestSend_AutomatedSyncNoUserActivity(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{})
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"hi","wait_none":true}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if userActivityRecorded(d, testSessionKey) {
		t.Error("automated sync send set last_user_activity_at; absent human must keep today's behaviour")
	}
}

// TestSend_HumanAsyncSetsUserActivity proves the marker survives the async
// dispatch path: the queued turn runs off the request context, and once it
// reaches the backend its entry write has stamped last_user_activity_at.
func TestSend_HumanAsyncSetsUserActivity(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mock.entered = make(chan string, 1)
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"async hi","human":true,"async":true,"wait_none":true}`)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}
	select {
	case <-mock.entered: // the turn reached the backend → its entry write ran
	case <-time.After(5 * time.Second):
		t.Fatal("queued turn never reached the backend")
	}
	if !userActivityRecorded(d, testSessionKey) {
		t.Error("human async send did not set last_user_activity_at once its turn ran")
	}
}

// TestSend_HumanSlashCommandSetsUserActivity proves req 5's second half: a
// human /send whose text is a slash command handled by the command dispatcher
// starts no turn, so the handler itself stamps last_user_activity_at for the
// target session.
func TestSend_HumanSlashCommandSetsUserActivity(t *testing.T) {
	ping := &command.Command{
		Name: "ping",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "pong"}, nil
		},
	}
	d, mock := httpTestSetup(t, httpTestOpts{commands: []*command.Command{ping}})
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"/ping","human":true,"wait_none":true}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] != "pong" {
		t.Fatalf("response = %q, want pong (command not dispatched?)", resp["response"])
	}
	if calls := mock.snapshot(); len(calls) != 0 {
		t.Fatalf("backend called %d time(s) for a slash command, want 0", len(calls))
	}
	if !userActivityRecorded(d, testSessionKey) {
		t.Error("human slash-command send did not set last_user_activity_at on the target session")
	}
}

// TestCommand_HumanSetsUserActivity proves req 5's first half: a human
// /command dispatch stamps last_user_activity_at on the command's target
// session (a command starts no turn, so nothing else would).
func TestCommand_HumanSetsUserActivity(t *testing.T) {
	ping := &command.Command{
		Name: "ping",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "pong"}, nil
		},
	}
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{ping}})
	mux := newTestMux(d)

	w := postJSON(mux, "/command", `{"command":"/ping","human":true}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if !userActivityRecorded(d, testSessionKey) {
		t.Error("human command did not set last_user_activity_at on the command's target session")
	}
}

// TestBranch_HumanSetsUserActivityOnBranchSession proves the #1130 branch
// path: a human /branch runs its first turn with the human-source marker, so
// the durable write lands on the NEW branch session key at turn entry, not
// the parent's.
func TestBranch_HumanSetsUserActivityOnBranchSession(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{})
	mux := newTestMux(d)

	w := postJSON(mux, "/branch", `{"text":"branch hi","human":true}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	branchKey := resp["session"]
	if !strings.HasPrefix(branchKey, testSessionKey+"/b") {
		t.Fatalf("session = %q, want a branch of %s", branchKey, testSessionKey)
	}
	if !userActivityRecorded(d, branchKey) {
		t.Errorf("human branch did not set last_user_activity_at on the branch session %q", branchKey)
	}
	if userActivityRecorded(d, testSessionKey) {
		t.Error("human branch stamped the PARENT session; the write belongs to the branch turn's own key")
	}
}

// TestSend_HumanGateSkippedNoUserActivity pins req 3's skip boundary: a
// human-declared send that an if_* gate SKIPS records no user activity — the
// declaration takes effect only when the request dispatches.
func TestSend_HumanGateSkippedNoUserActivity(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	d.sessionIndex.TouchCacheTouch(testSessionKey, time.Now()) // warm → if_inactive trips
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"hi","human":true,"if_inactive":"1h"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] != "skipped: session recently active" {
		t.Fatalf("response = %q, want the skip message", resp["response"])
	}
	if userActivityRecorded(d, testSessionKey) {
		t.Error("gate-skipped human send recorded user activity; a skipped request must not")
	}
	if calls := mock.snapshot(); len(calls) != 0 {
		t.Errorf("backend called %d time(s) for a skipped send, want 0", len(calls))
	}
}

// TestSend_HumanWaitDeferredNoUserActivity pins req 3's defer boundary: a
// human-declared send that a wait_* gate DEFERS records no user activity, and
// the declaration must not satisfy the wait condition itself.
func TestSend_HumanWaitDeferredNoUserActivity(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{})
	withDeferStore(t, &d)
	mux := newTestMux(d)

	// Session cold → wait_warm unmet → deferred. A human declaration on the
	// same request must not count as the warmth it waits for.
	w := postJSON(mux, "/send", `{"text":"hi","human":true,"wait_warm":"1h"}`)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 deferred; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "deferred" {
		t.Fatalf("status = %v, want deferred", resp["status"])
	}
	if userActivityRecorded(d, testSessionKey) {
		t.Error("wait-deferred human send recorded user activity; a deferred request must not")
	}
}

// TestSweep_DeferredHumanDeliveredAutomated pins req 8: the human flag is not
// part of the deferred record, so the sweep delivers the request as
// automated — no user activity is recorded at delivery time.
func TestSweep_DeferredHumanDeliveredAutomated(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mock.entered = make(chan string, 1)
	store := withDeferStore(t, &d)
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"later","human":true,"wait_warm":"1h"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 deferred; body: %s", w.Code, w.Body.String())
	}
	// The wait condition holds only now (session warmed after the defer), so
	// the next sweep delivers — the moment req 8 judges.
	d.sessionIndex.TouchCacheTouch(testSessionKey, time.Now())

	sweepFor(d, store).sweep()

	select {
	case text := <-mock.entered:
		if !strings.HasSuffix(text, "later") {
			t.Errorf("delivered text = %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deferred send was not delivered by the sweep")
	}
	if userActivityRecorded(d, testSessionKey) {
		t.Error("sweep delivery of a human-enqueued send recorded user activity; deferred delivery is automated")
	}
}

// TestSend_HumanTriggerStillUser pins req 7: a human send keeps trigger
// "user" — the declaration widens only the user-activity write, never the
// turn's classification (steering, shutdown refusal, platform tagging).
func TestSend_HumanTriggerStillUser(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"hi","human":true,"wait_none":true}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if calls := mock.snapshot(); len(calls) != 1 || calls[0].trigger != "user" {
		t.Errorf("calls = %+v, want one call with trigger %q", calls, "user")
	}
}
