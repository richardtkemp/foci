package agent

import (
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/relogin"
)

// claimRelogin claims the process-wide re-login gate for one test and releases
// it on cleanup, so a failing assertion can never leave it active (or a login
// pending, #2384) for the next test in the package.
func claimRelogin(t *testing.T) {
	t.Helper()
	if !relogin.G.Start() {
		t.Fatal("re-login gate already active at test start")
	}
	t.Cleanup(func() {
		relogin.G.Release()
		relogin.G.Complete()
	})
}

// TestInbox_Relogin_EnqueueAcceptsAndHolds is the #1932 regression: a message
// to a delegated agent while a CC re-login is in flight used to be dropped at
// Enqueue (return false, never queued) after the front end had already shown
// it as sent. It must be accepted, and the worker must hold it — no turn
// dispatched against the unauthenticated backend — until the gate releases.
func TestInbox_Relogin_EnqueueAcceptsAndHolds(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.DelegatedManager = &DelegatedManager{}

	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)

	claimRelogin(t)
	if !a.Enqueue(Envelope{SessionKey: "test/s", Text: "during relogin", Driver: d}) {
		t.Fatal("Enqueue rejected a message during re-login; it must be held, not dropped")
	}
	if waitFor(300*time.Millisecond, func() bool { return d.NumCalls() > 0 }) {
		t.Fatalf("worker dispatched while re-login active; calls=%d", d.NumCalls())
	}

	relogin.G.Release()
	if !waitFor(time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("held message not dispatched after re-login released; calls=%d", d.NumCalls())
	}
	if got := d.Calls()[0]; len(got) != 1 || got[0].Text != "during relogin" {
		t.Errorf("dispatched batch = %+v, want the held message", got)
	}
}

// TestInbox_Relogin_HoldsInjection: system injections (cron, keepalive,
// inter-session notify) are held the same way — running one would drive a turn
// into the backend that cannot authenticate.
func TestInbox_Relogin_HoldsInjection(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.DelegatedManager = &DelegatedManager{}

	claimRelogin(t)
	var ran atomic.Bool
	if !a.Enqueue(Envelope{SessionKey: "test/s", Inject: &InjectMeta{Trigger: "test", Run: func() { ran.Store(true) }}}) {
		t.Fatal("Enqueue rejected an injection during re-login")
	}
	if waitFor(300*time.Millisecond, ran.Load) {
		t.Fatal("injection ran while re-login active")
	}
	// A successful login releases the gate and ends the pending state.
	relogin.G.Complete()
	relogin.G.Release()
	if !waitFor(time.Second, ran.Load) {
		t.Fatal("held injection did not run after re-login completed")
	}
}

// TestInbox_Relogin_InjectionNotCapturedAsCode is the #2384 regression: with
// the capture window open, a system injection (an HTTP /send, a proactive
// warning) arrived first and was taken as the login code. Its Text is empty,
// so the driver submitted a 0-char code and the attempt failed. Only a user
// message may be the code; the injection is held.
func TestInbox_Relogin_InjectionNotCapturedAsCode(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.AgentID = "clutch"
	a.DelegatedManager = &DelegatedManager{}

	claimRelogin(t)
	relogin.G.OpenCapture("clutch")
	var ran atomic.Bool
	if !a.Enqueue(Envelope{SessionKey: "test/s", Inject: &InjectMeta{Trigger: "user", Run: func() { ran.Store(true) }}}) {
		t.Fatal("Enqueue rejected an injection during re-login")
	}
	if !relogin.G.ShouldCapture("clutch") {
		t.Fatal("injection consumed the capture window; it must stay open for the user's code")
	}
	if !a.Enqueue(Envelope{SessionKey: "test/s", Text: "the-code"}) {
		t.Fatal("capture should report the user's message as consumed")
	}
	if code, ok := relogin.G.AwaitCode(time.Second); !ok || code != "the-code" {
		t.Fatalf("AwaitCode = %q,%v; want the-code,true", code, ok)
	}
	if ran.Load() {
		t.Fatal("injection ran while re-login active")
	}
}

// TestInbox_Relogin_AbortKeepsInjectionsPaused (#2384): after an attempt fails
// the gate releases, but the login is still owed. An injection run then would
// 401 and start another attempt with a new link (the overnight loop), so it is
// paused until the login completes, while a user message still dispatches.
func TestInbox_Relogin_AbortKeepsInjectionsPaused(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.DelegatedManager = &DelegatedManager{}

	claimRelogin(t)
	var ran atomic.Bool
	a.Enqueue(Envelope{SessionKey: "test/s", Inject: &InjectMeta{Trigger: "warnings", Run: func() { ran.Store(true) }}})
	relogin.G.Release() // the attempt aborted: no Complete
	if waitFor(300*time.Millisecond, ran.Load) {
		t.Fatal("injection ran after a failed attempt while the login is still pending")
	}
	relogin.G.Complete()
	if !waitFor(time.Second, ran.Load) {
		t.Fatal("paused injection did not run after the login completed")
	}
}

// TestInbox_Relogin_UserTurnEndsPending (#2384): a user turn that runs to the
// end with no new attempt starting shows the credential works (renewed outside
// foci, say), so the pending state ends and paused injections run.
func TestInbox_Relogin_UserTurnEndsPending(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.DelegatedManager = &DelegatedManager{}

	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)

	claimRelogin(t)
	var ran atomic.Bool
	a.Enqueue(Envelope{SessionKey: "test/s", Inject: &InjectMeta{Trigger: "warnings", Run: func() { ran.Store(true) }}})
	relogin.G.Release()
	if waitFor(200*time.Millisecond, ran.Load) {
		t.Fatal("injection ran while the login is still pending")
	}
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "hello", Driver: d})
	if !waitFor(time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("user message not dispatched while a login is pending; calls=%d", d.NumCalls())
	}
	if !waitFor(time.Second, ran.Load) {
		t.Fatal("paused injection did not run after a user turn showed the credential works")
	}
	if relogin.G.Pending() {
		t.Fatal("login still pending after a user turn completed with no new attempt")
	}
}

// TestInbox_Relogin_CaptureStillDiverts: the capture window is unchanged — the
// triggering agent's next message is the login code, consumed, never queued.
func TestInbox_Relogin_CaptureStillDiverts(t *testing.T) {
	a := newTestAgent(t)
	a.AgentID = "clutch"
	a.DelegatedManager = &DelegatedManager{}

	claimRelogin(t)
	relogin.G.OpenCapture("clutch")
	if !a.Enqueue(Envelope{SessionKey: "test/s", Text: "the-code"}) {
		t.Fatal("capture should report the message as consumed")
	}
	if inb := a.lookupInbox("test/s"); inb != nil && len(inb.drainAvailable()) != 0 {
		t.Fatal("captured login code must not also be queued as a turn")
	}
	if code, ok := relogin.G.AwaitCode(time.Second); !ok || code != "the-code" {
		t.Fatalf("AwaitCode = %q,%v; want the-code,true", code, ok)
	}
}
