package agent

import (
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/relogin"
	"foci/internal/usagehold"
)

// The Claude Code login holds only agents that use the Claude Code credential
// (#3014, Dick 2026-11-10). A delegated agent on another backend (codex,
// opencode: no ReloginTrigger, because its spec lacks CapRelogin) is not held
// by the inbox wait, the injection pause or availability, and its user turns
// do not end the CC agents' pending login.

// otherBackendAgent is a started delegated agent on a backend without the
// Claude Code login.
func otherBackendAgent(t *testing.T) (*Agent, func()) {
	t.Helper()
	a, cancel := startedAgent(t)
	a.AgentID = "codexer"
	a.DelegatedManager = &DelegatedManager{BackendType: "codex"}
	return a, cancel
}

// TestRelogin_OtherBackend_UserMessageNotHeld: a user message to a codex agent
// dispatches while a CC login attempt runs, and is never taken as the code.
func TestRelogin_OtherBackend_UserMessageNotHeld(t *testing.T) {
	a, cancel := otherBackendAgent(t)
	defer cancel()
	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)

	claimRelogin(t)
	relogin.G.OpenCapture("", "", "")
	if !a.Enqueue(Envelope{SessionKey: "codexer/s", Text: "hello", Driver: d}) {
		t.Fatal("Enqueue rejected the message")
	}
	if !waitFor(5*time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("a codex agent's message was held by the CC login; calls=%d", d.NumCalls())
	}
	if !relogin.G.Active() {
		t.Fatal("the CC attempt ended")
	}
}

// TestRelogin_OtherBackend_InjectionNotPaused: an injection on a codex agent
// runs while a CC login is pending after a failed attempt.
func TestRelogin_OtherBackend_InjectionNotPaused(t *testing.T) {
	a, cancel := otherBackendAgent(t)
	defer cancel()

	claimRelogin(t)
	relogin.G.Release() // the attempt failed: the login is still owed
	var ran atomic.Bool
	a.Enqueue(Envelope{SessionKey: "codexer/s", Inject: &InjectMeta{Trigger: "warnings", Run: func() { ran.Store(true) }}})
	if !waitFor(5*time.Second, ran.Load) {
		t.Fatal("a codex agent's injection was paused by the pending CC login")
	}
}

// TestRelogin_OtherBackend_TurnKeepsPending: a codex user turn that runs to
// the end says nothing about the CC credential, so the CC login stays owed.
func TestRelogin_OtherBackend_TurnKeepsPending(t *testing.T) {
	a, cancel := otherBackendAgent(t)
	defer cancel()
	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)

	claimRelogin(t)
	relogin.G.Release()
	a.Enqueue(Envelope{SessionKey: "codexer/s", Text: "first", Driver: d})
	if !waitFor(5*time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("turn did not run; calls=%d", d.NumCalls())
	}
	// The worker is serial and ends pending after a turn returns: once a
	// second turn has dispatched, the first turn's post-turn step has run.
	a.Enqueue(Envelope{SessionKey: "codexer/s", Text: "second", Driver: d})
	if !waitFor(5*time.Second, func() bool { return d.NumCalls() == 2 }) {
		t.Fatalf("second turn did not run; calls=%d", d.NumCalls())
	}
	if !relogin.G.Pending() {
		t.Fatal("a codex turn ended the pending CC login")
	}
}

// TestUnavailable_OtherBackendDuringLogin: a codex agent is available while a
// CC login runs; a CC agent is not.
func TestUnavailable_OtherBackendDuringLogin(t *testing.T) {
	reg := usagehold.New()
	cc := holdAgent(reg, "claude-code")
	onCCLogin(cc)
	other := holdAgent(reg, "codex")

	claimRelogin(t)
	if why, un := other.Unavailable(); un {
		t.Errorf("a codex agent in a CC login: unavailable %q, want available", why)
	}
	if _, un := cc.Unavailable(); !un {
		t.Error("a CC agent in a CC login: available, want unavailable")
	}
}
