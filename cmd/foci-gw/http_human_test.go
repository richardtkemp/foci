package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/periodic"
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

// TestSend_HumanSendStampsInProcessReceipt pins req 6 on both /send dispatch
// shapes: a human send that runs (sync) or is queued (async) stamps the
// periodic runner's in-process receipt, and an automated one does not. The
// durable timestamp is written by the turn either way, so only this test
// notices if either notifyHumanInteraction call in handleSend is lost (#2230).
func TestSend_HumanSendStampsInProcessReceipt(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantCode   int
		wantStamp  bool
	}{
		{"sync human", `{"text":"hi","human":true,"wait_none":true}`, http.StatusOK, true},
		{"async human", `{"text":"hi","human":true,"async":true,"wait_none":true}`, http.StatusAccepted, true},
		{"sync automated", `{"text":"hi","wait_none":true}`, http.StatusOK, false},
		{"async automated", `{"text":"hi","async":true,"wait_none":true}`, http.StatusAccepted, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &periodic.Runner{}
			d, _ := httpTestSetup(t, httpTestOpts{kaRunner: runner})
			mux := newTestMux(d)

			w := postJSON(mux, "/send", tc.body)

			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, tc.wantCode, w.Body.String())
			}
			if _, ok := runner.LastUserActivity(); ok != tc.wantStamp {
				t.Errorf("in-process receipt stamped = %v, want %v", ok, tc.wantStamp)
			}
		})
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

// TestCommand_HumanStampsOnlyWhenDispatched pins the dispatch boundary of the
// human declaration on /command: the stamps belong to a command actually
// being handled. A registered command with no Execute function fails to
// dispatch (the endpoint's only 404) and records nothing, while the registry
// ANSWERING an unknown name is a dispatch — a human typo is still human
// attention and stamps.
func TestCommand_HumanStampsOnlyWhenDispatched(t *testing.T) {
	broken := &command.Command{Name: "broken"} // registered, no Execute → Dispatch reports not-dispatched
	d, _ := httpTestSetup(t, httpTestOpts{commands: []*command.Command{broken}})
	mux := newTestMux(d)

	w := postJSON(mux, "/command", `{"command":"/broken","human":true}`)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
	if userActivityRecorded(d, testSessionKey) {
		t.Error("human command that failed to dispatch recorded user activity; the stamp belongs to a dispatched command")
	}

	w = postJSON(mux, "/command", `{"command":"/nosuch","human":true}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !strings.Contains(resp["response"], "Unknown command") {
		t.Fatalf("response = %q, want the registry's unknown-name answer", resp["response"])
	}
	if !userActivityRecorded(d, testSessionKey) {
		t.Error("human command answered by the registry did not set last_user_activity_at")
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

// TestBranch_HumanReceiptStampsOnlyWhenDispatched pins the dispatch boundary
// of the #1130 in-process receipt on /branch: the receipt belongs to a branch
// that actually ran, not to a request that merely passed its gates. A bad
// model override is refused inside runBranchTurn — after every gate, before
// any turn exists — and stamps neither receipt, while a human branch that
// runs stamps the runner receipt (the durable timestamp stays the branch
// turn's own entry write, on the new session key).
func TestBranch_HumanReceiptStampsOnlyWhenDispatched(t *testing.T) {
	runner := &periodic.Runner{}
	d, _ := httpTestSetup(t, httpTestOpts{kaRunner: runner})
	mux := newTestMux(d)

	w := postJSON(mux, "/branch", `{"text":"hi","human":true,"model":"cheap"}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	if _, ok := runner.LastUserActivity(); ok {
		t.Error("human branch refused for a bad model stamped the in-process receipt; the stamp belongs to a dispatched run")
	}
	if userActivityRecorded(d, testSessionKey) {
		t.Error("human branch refused for a bad model recorded durable user activity on the parent session")
	}

	w = postJSON(mux, "/branch", `{"text":"hi","human":true}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if _, ok := runner.LastUserActivity(); !ok {
		t.Error("dispatched human branch did not stamp the in-process receipt")
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

// TestSend_HumanUserGateReadsBeforeOwnStamp pins the #1130 gate ordering on
// the user-activity gates themselves: the declaration is applied only after
// the request's own gates were read, so it can never satisfy its own
// if_user_active — the first request still skips on a session with no user
// activity. The stamp a DISPATCHED human request leaves behind is durable,
// though: a later request's if_user_active gate sees it and runs.
func TestSend_HumanUserGateReadsBeforeOwnStamp(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mux := newTestMux(d)

	// No user activity yet → if_user_active trips. The request's own
	// declaration must not be the activity it conditions on.
	w := postJSON(mux, "/send", `{"text":"hi","human":true,"if_user_active":"1h"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] != "skipped: no recent user activity" {
		t.Fatalf("response = %q, want the if_user_active skip message", resp["response"])
	}
	if userActivityRecorded(d, testSessionKey) {
		t.Error("gate-skipped human send recorded user activity; its own declaration must not satisfy its own gate")
	}
	if calls := mock.snapshot(); len(calls) != 0 {
		t.Errorf("backend called %d time(s) for a skipped send, want 0", len(calls))
	}

	// A human send that dispatches stamps the session…
	w = postJSON(mux, "/send", `{"text":"again","human":true,"wait_none":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if !userActivityRecorded(d, testSessionKey) {
		t.Fatal("dispatched human send did not set last_user_activity_at")
	}

	// …so the SAME gate now holds for a later request, which runs.
	w = postJSON(mux, "/send", `{"text":"once more","human":true,"if_user_active":"1h"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	resp = map[string]string{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] == "skipped: no recent user activity" {
		t.Error("later if_user_active send skipped despite the earlier human request's stamp")
	}
	if calls := mock.snapshot(); len(calls) != 2 {
		t.Errorf("backend calls = %d, want 2 (the skipped request never ran)", len(calls))
	}
}

// TestSend_HumanWaitUserGateReadsBeforeOwnStamp is the wait-gate half of the
// #1130 ordering: a request's own human declaration must not satisfy its own
// wait_user_active — it defers on a session with no user activity — while
// the stamp from an earlier dispatched human request releases a later one
// immediately.
func TestSend_HumanWaitUserGateReadsBeforeOwnStamp(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	withDeferStore(t, &d)
	mux := newTestMux(d)

	// No user activity yet → wait_user_active unmet → deferred. The
	// request's own declaration must not be the activity it waits for.
	w := postJSON(mux, "/send", `{"text":"first","human":true,"wait_user_active":"1h"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 deferred; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "deferred" {
		t.Fatalf("status = %v, want deferred", resp["status"])
	}
	if userActivityRecorded(d, testSessionKey) {
		t.Error("wait-deferred human send recorded user activity; its own declaration must not satisfy its own wait")
	}

	// A human send that dispatches stamps the session…
	w = postJSON(mux, "/send", `{"text":"second","human":true,"wait_none":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if !userActivityRecorded(d, testSessionKey) {
		t.Fatal("dispatched human send did not set last_user_activity_at")
	}

	// …so the SAME wait now holds and the next request runs immediately
	// (the deferred one stays queued until a sweep).
	w = postJSON(mux, "/send", `{"text":"third","human":true,"wait_user_active":"1h"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (wait held by the earlier stamp); body: %s", w.Code, w.Body.String())
	}
	if calls := mock.snapshot(); len(calls) != 2 {
		t.Errorf("backend calls = %d, want 2 (the deferred request stays queued)", len(calls))
	}
}

// TestSend_HumanIfUserInactiveRuns pins the mirror side of the #1130
// ordering: the declaration is not written before the gates are read, so it
// cannot flip its own if_user_inactive gate to "user recently active" — a
// human send to a session with no user activity RUNS, and only its dispatch
// stamps the session.
func TestSend_HumanIfUserInactiveRuns(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"hi","human":true,"if_user_inactive":"1h"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] == "skipped: user recently active" {
		t.Fatalf("response = %q; the request's own declaration must not block its own if_user_inactive gate", resp["response"])
	}
	if calls := mock.snapshot(); len(calls) != 1 {
		t.Fatalf("backend calls = %d, want 1 (the gated send must run)", len(calls))
	}
	if !userActivityRecorded(d, testSessionKey) {
		t.Error("human send that ran under if_user_inactive did not set last_user_activity_at")
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

// newPingCommand is the ping command the automated-declaration tests use as
// their no-turn dispatch shape: it answers "pong" and touches nothing, so any
// receipt observed alongside it can only come from the endpoint's own #1130
// stamps, never from the command or a turn.
func newPingCommand() *command.Command {
	return &command.Command{
		Name: "ping",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "pong"}, nil
		},
	}
}

// runnerReceiptStamped is the in-process mirror of userActivityRecorded: it
// reports whether the agent's periodic runner holds the "a human interacted"
// receipt — #1130's second, non-durable stamp.
func runnerReceiptStamped(runner *periodic.Runner) bool {
	_, ok := runner.LastUserActivity()
	return ok
}

// assertNoHumanReceipts asserts the automated half of the #1130 declaration:
// neither receipt exists — no durable last_user_activity_at on any of
// sessionKeys, and no in-process receipt on the runner. why names the case's
// reason in the failure message ("explicit human:false", "omitted human
// key", "wait-deferred").
func assertNoHumanReceipts(t *testing.T, endpoint, caseName, why string, d httpHandlerDeps, runner *periodic.Runner, sessionKeys ...string) {
	t.Helper()
	for _, key := range sessionKeys {
		if userActivityRecorded(d, key) {
			t.Errorf("%s %s: %s stamped last_user_activity_at on session %q", endpoint, caseName, why, key)
		}
	}
	if runnerReceiptStamped(runner) {
		t.Errorf("%s %s: %s stamped the in-process runner receipt", endpoint, caseName, why)
	}
}

// dispatchProof asserts, inside a subtest, that the request really dispatched
// — so a negative receipt assertion cannot pass because nothing ran — and
// returns any session keys beyond testSessionKey that the human version of
// the same request would stamp (the new branch key for /branch).
type dispatchProof func(t *testing.T, endpoint, caseName string, w *httptest.ResponseRecorder, mock *mockClient) []string

// proveTurnDispatched is the /send dispatch proof: the request ran exactly
// one backend turn. The turn stamps only its own session, so no extra keys.
func proveTurnDispatched(t *testing.T, endpoint, caseName string, _ *httptest.ResponseRecorder, mock *mockClient) []string {
	t.Helper()
	if calls := mock.snapshot(); len(calls) != 1 {
		t.Fatalf("%s %s: backend calls = %d, want 1 (the send's turn must run)", endpoint, caseName, len(calls))
	}
	return nil
}

// proveSlashCommandAnswered is the slash-command dispatch proof: the command
// answered "pong" and no backend turn ran — dispatched, and with no turn
// whose entry write could stamp either receipt on the stamps' behalf.
func proveSlashCommandAnswered(t *testing.T, endpoint, caseName string, w *httptest.ResponseRecorder, mock *mockClient) []string {
	t.Helper()
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] != "pong" {
		t.Fatalf("%s %s: response = %q, want pong (command not dispatched?)", endpoint, caseName, resp["response"])
	}
	if calls := mock.snapshot(); len(calls) != 0 {
		t.Fatalf("%s %s: backend called %d time(s), want 0", endpoint, caseName, len(calls))
	}
	return nil
}

// proveBranchSessionCreated is the /branch dispatch proof: the response
// carries a new branch session key. The human version of the same request
// stamps THAT key, so it is returned for the caller to assert on too.
func proveBranchSessionCreated(t *testing.T, endpoint, caseName string, w *httptest.ResponseRecorder, _ *mockClient) []string {
	t.Helper()
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	branchKey := resp["session"]
	if !strings.HasPrefix(branchKey, testSessionKey+"/b") {
		t.Fatalf("%s %s: session = %q, want a branch of %s", endpoint, caseName, branchKey, testSessionKey)
	}
	return []string{branchKey}
}

// automatedCase is one row of the automated-declaration tables (#2230): a
// request body on an endpoint plus its dispatch proof.
type automatedCase struct {
	name     string
	endpoint string
	body     string
	proof    dispatchProof
}

// runAutomatedCase drives one automated-declaration case end to end: a fresh
// harness with the runner wired and ping registered, the POST, the 200, the
// dispatch proof, then the assertion that neither #1130 receipt was left on
// the target session(s) or the runner.
func runAutomatedCase(t *testing.T, tc automatedCase, why string) {
	t.Helper()
	runner := &periodic.Runner{}
	d, mock := httpTestSetup(t, httpTestOpts{kaRunner: runner, commands: []*command.Command{newPingCommand()}})
	mux := newTestMux(d)

	w := postJSON(mux, tc.endpoint, tc.body)
	if w.Code != http.StatusOK {
		t.Fatalf("%s %s: status = %d, want 200; body: %s", tc.endpoint, tc.name, w.Code, w.Body.String())
	}
	keys := tc.proof(t, tc.endpoint, tc.name, w, mock)
	assertNoHumanReceipts(t, tc.endpoint, tc.name, why, d, runner, append([]string{testSessionKey}, keys...)...)
}

// TestEndpoints_ExplicitHumanFalseIsAutomated pins #2230 req 1: an explicit
// "human":false is a declaration of automation, not a no-op key. On every
// endpoint that takes the field the request still dispatches (each case
// proves it) and leaves neither receipt — no last_user_activity_at on the
// session the human version would stamp (for /branch: the new branch session
// AND the parent), no in-process runner receipt.
func TestEndpoints_ExplicitHumanFalseIsAutomated(t *testing.T) {
	for _, tc := range []automatedCase{
		{"send", "/send", `{"text":"hi","human":false,"wait_none":true}`, proveTurnDispatched},
		{"send slash command", "/send", `{"text":"/ping","human":false,"wait_none":true}`, proveSlashCommandAnswered},
		{"command", "/command", `{"command":"/ping","human":false}`, proveSlashCommandAnswered},
		{"branch", "/branch", `{"text":"branch hi","human":false}`, proveBranchSessionCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runAutomatedCase(t, tc, "explicit human:false")
		})
	}
}

// TestEndpoints_AbsentHumanKeyIsAutomated pins #2230 req 2: omitting the
// human key keeps today's automated behaviour on the no-turn paths and on
// /branch — the decode-to-false default must stamp nothing. Each case proves
// the request dispatched, so the negative assertions cannot pass vacuously.
func TestEndpoints_AbsentHumanKeyIsAutomated(t *testing.T) {
	for _, tc := range []automatedCase{
		{"send slash command", "/send", `{"text":"/ping","wait_none":true}`, proveSlashCommandAnswered},
		{"command", "/command", `{"command":"/ping"}`, proveSlashCommandAnswered},
		{"branch", "/branch", `{"text":"branch hi"}`, proveBranchSessionCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runAutomatedCase(t, tc, "omitted human key")
		})
	}
}

// TestEndpoints_HumanNoTurnPathsStampBothReceipts pins #2230 req 3: a human
// /command dispatch and a human /send whose text is a slash command start no
// turn, so stampHumanCommandDispatch must leave BOTH receipts — the durable
// last_user_activity_at on the target session and the in-process runner
// receipt. Zero backend calls prove no turn ran, so neither stamp can come
// from a turn's entry write.
func TestEndpoints_HumanNoTurnPathsStampBothReceipts(t *testing.T) {
	for _, tc := range []struct{ name, endpoint, body string }{
		{"command", "/command", `{"command":"/ping","human":true}`},
		{"send slash command", "/send", `{"text":"/ping","human":true,"wait_none":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &periodic.Runner{}
			d, mock := httpTestSetup(t, httpTestOpts{kaRunner: runner, commands: []*command.Command{newPingCommand()}})
			mux := newTestMux(d)

			w := postJSON(mux, tc.endpoint, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("%s %s: status = %d, want 200; body: %s", tc.endpoint, tc.name, w.Code, w.Body.String())
			}
			proveSlashCommandAnswered(t, tc.endpoint, tc.name, w, mock)
			if !userActivityRecorded(d, testSessionKey) {
				t.Errorf("%s %s: human dispatch did not set last_user_activity_at on the target session", tc.endpoint, tc.name)
			}
			if !runnerReceiptStamped(runner) {
				t.Errorf("%s %s: human dispatch did not stamp the in-process runner receipt", tc.endpoint, tc.name)
			}
		})
	}
}

// TestEndpoints_HumanDeferredByWaitStampsNothing pins #2230 req 4: a human
// /command or /branch that a wait_* gate defers records neither receipt —
// the stamps belong to the dispatch boundary, which a deferred request has
// not crossed (the sweep later delivers it as automated). The session stays
// cold so wait_warm is unmet; no branch session exists at defer time, so the
// durable check covers both the command's target and the branch's parent:
// testSessionKey.
func TestEndpoints_HumanDeferredByWaitStampsNothing(t *testing.T) {
	for _, tc := range []struct{ name, endpoint, body string }{
		{"command", "/command", `{"command":"/ping","human":true,"wait_warm":"1h"}`},
		{"branch", "/branch", `{"text":"branch hi","human":true,"wait_warm":"1h"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &periodic.Runner{}
			d, _ := httpTestSetup(t, httpTestOpts{kaRunner: runner, commands: []*command.Command{newPingCommand()}})
			withDeferStore(t, &d)
			mux := newTestMux(d)

			w := postJSON(mux, tc.endpoint, tc.body)
			if w.Code != http.StatusAccepted {
				t.Fatalf("%s %s: status = %d, want 202 deferred; body: %s", tc.endpoint, tc.name, w.Code, w.Body.String())
			}
			var resp map[string]interface{}
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if resp["status"] != "deferred" {
				t.Fatalf("%s %s: status = %v, want deferred", tc.endpoint, tc.name, resp["status"])
			}
			assertNoHumanReceipts(t, tc.endpoint, tc.name, "wait-deferred", d, runner, testSessionKey)
		})
	}
}
