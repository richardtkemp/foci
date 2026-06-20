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
// The gate is a DROP gate, not a queue: while a re-login is in progress, inbound
// user messages for delegated agents are ignored, except for one capture window
// where the next message from the triggering agent is taken as the login code.
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
	captureAgentID string      // when non-empty, the agent whose next inbound message is the login code
	codeCh         chan string // buffered(1); delivers the captured code to the driver
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
	g.captureAgentID = ""
	g.codeCh = make(chan string, 1)
	return true
}

// Active reports whether a re-login is in progress (delegated-agent input
// should be dropped).
func (g *Gate) Active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active
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

// Release ends the re-login, resuming normal message processing. Safe to call
// multiple times; the driver defers it as the unconditional backstop so a
// failed login can never leave the gate stuck active.
func (g *Gate) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active = false
	g.captureAgentID = ""
	g.codeCh = nil
}
