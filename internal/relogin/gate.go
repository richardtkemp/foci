// Package relogin coordinates automated Claude Code (CC) re-authentication.
//
// All claude-code-backed agents share a single OAuth credential at
// ~/.claude/.credentials.json. When that token can no longer be refreshed the
// `claude` subprocess returns a 401 ("Failed to authenticate"). foci's proactive
// refresh (internal/anthropic/cctoken.go) only covers the still-valid-refresh-
// token case; a genuine re-login needs a human to sign in. This package detects
// nothing itself — the ccstream backend calls a hook on 401 — but it owns the
// coordination: a process-wide gate that pauses inbound message processing for
// every delegated agent, and a tmux-driven login flow that relays the sign-in
// URL to the user and feeds back the pasted code.
//
// The gate is a HOLD gate, not a queue of its own: while a re-login is in
// progress, delegated agents' session inbox workers hold dispatch (inbound
// messages keep queuing on their inbox channels) until Released fires, except
// for one capture window where the next message from the triggering agent is
// taken as the login code.
//
// The gate also tracks a wider "login pending" state (#2384): it begins with
// the first Start and lasts across failed attempts until a login completes
// (Complete) or a later user turn shows the credential works again
// (ClearPendingIf). An attempt that aborts releases the hold on user messages,
// but system injections stay paused for the whole pending window: run against
// the dead credential, each one 401s and starts another attempt with a new
// sign-in link, which is how one expired token became ~30 links overnight.
package relogin

import (
	"sync"
	"time"
)

// Gate is the process-wide singleton that serializes CC re-login. One OAuth
// credential is shared across all claude-code agents, so the gate is global,
// not per-agent: the first 401 claims it (single-flight) and runs the driver;
// concurrent 401s see it already active and no-op.
type Gate struct {
	mu             sync.Mutex
	active         bool
	captureAgentID string        // when non-empty, the agent whose next inbound message is the login code
	codeCh         chan string   // buffered(1); delivers the captured code to the driver
	released       chan struct{} // closed by Release; nil while inactive

	pending    bool         // a login is owed: set by Start, cleared by Complete / ClearPendingIf
	attempt    uint64       // incremented by every successful Start
	onComplete []func()     // run (outside mu) each time pending clears
	link       *linkMessage // the sign-in link message of this pending login (link.go); cleared with pending
}

// G is the process-wide re-login gate.
var G = &Gate{}

// Start attempts to claim the gate for a re-login. Returns true if claimed (the
// caller should run the driver); false if a re-login is already in progress.
func (g *Gate) Start() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active {
		return false
	}
	g.active = true
	g.pending = true
	g.attempt++
	g.captureAgentID = ""
	g.codeCh = make(chan string, 1)
	g.released = make(chan struct{})
	return true
}

// Pending reports whether a login is owed: an attempt is running, or the last
// one failed and nothing has shown the credential working since. Delegated
// agents pause system injections while it is true (#2384).
func (g *Gate) Pending() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pending
}

// PendingAttempt returns the current attempt number and whether a login is
// pending. A caller about to run a user turn records it and passes the number
// to ClearPendingIf afterwards.
func (g *Gate) PendingAttempt() (uint64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.attempt, g.pending
}

// ClearPendingIf ends the pending state when no attempt is running and none
// has started since attempt was read: a user turn ran to the end without a
// 401, so the credential works (for example, it was renewed outside foci).
// Without it a login completed elsewhere would pause injections until the
// next restart. Reports whether it cleared.
func (g *Gate) ClearPendingIf(attempt uint64) bool {
	g.mu.Lock()
	if !g.pending || g.active || g.attempt != attempt {
		g.mu.Unlock()
		return false
	}
	return g.clearPendingLocked()
}

// Complete marks the login as done: the pending state ends and the OnComplete
// callbacks run, so paused injections resume. The driver calls it on success.
func (g *Gate) Complete() {
	g.mu.Lock()
	if !g.pending {
		g.mu.Unlock()
		return
	}
	g.clearPendingLocked()
}

// clearPendingLocked clears pending, unlocks mu and runs the callbacks.
func (g *Gate) clearPendingLocked() bool {
	g.pending = false
	g.link = nil
	cbs := append([]func(){}, g.onComplete...)
	g.mu.Unlock()
	for _, fn := range cbs {
		fn()
	}
	return true
}

// OnComplete registers fn to run each time the pending state clears. Each
// delegated agent registers the drain of its paused injections.
func (g *Gate) OnComplete(fn func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onComplete = append(g.onComplete, fn)
}

// Active reports whether a re-login is in progress (delegated-agent dispatch
// should be held).
func (g *Gate) Active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active
}

// Released returns a channel closed when the in-progress re-login ends. With no
// re-login in progress it returns an already-closed channel, so a caller that
// read Active()==true and raced a Release never blocks on a dead channel.
func (g *Gate) Released() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.released == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return g.released
}

// OpenCapture opens the code-capture window for agentID: the next inbound
// message from that agent is taken as the login code (step 7).
func (g *Gate) OpenCapture(agentID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.captureAgentID = agentID
}

// ShouldCapture reports whether an inbound message from agentID should be
// diverted as the login code rather than dropped.
func (g *Gate) ShouldCapture(agentID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active && g.captureAgentID != "" && g.captureAgentID == agentID
}

// SubmitCode delivers a captured login code to the waiting driver and closes
// the capture window. Non-blocking: a second submission while one is buffered
// (or with no driver waiting) is dropped.
func (g *Gate) SubmitCode(code string) {
	g.mu.Lock()
	ch := g.codeCh
	g.captureAgentID = ""
	g.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- code:
	default:
	}
}

// AwaitCode blocks until a code is submitted or timeout elapses. The second
// return is false on timeout.
func (g *Gate) AwaitCode(timeout time.Duration) (string, bool) {
	g.mu.Lock()
	ch := g.codeCh
	g.mu.Unlock()
	if ch == nil {
		return "", false
	}
	select {
	case code := <-ch:
		return code, true
	case <-time.After(timeout):
		return "", false
	}
}

// Release ends the re-login attempt, resuming user-message processing. It does
// NOT end the pending state: after a failed attempt, injections stay paused
// until Complete or ClearPendingIf. Safe to call
// multiple times; the driver defers it as the unconditional backstop so a
// failed login can never leave the gate stuck active.
func (g *Gate) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active = false
	g.captureAgentID = ""
	g.codeCh = nil
	if g.released != nil {
		close(g.released)
		g.released = nil
	}
}
