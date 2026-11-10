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
	mu       sync.Mutex
	active   bool
	codeCh   chan string   // buffered(1); delivers the captured code to the driver
	released chan struct{} // closed by Release; nil while inactive

	// The capture window of the running attempt (#3011). While open, a user
	// message in a chat that has the sign-in link is the login code, and one
	// in any other chat gets the same link repeated there. All reset by
	// startLocked and Release.
	captureOpen bool
	linkText    string                // the link message text, repeated to another chat
	chats       map[chatRef]*linkChat // chats that have (or are being sent) this attempt's link
	codeChat    *linkChat             // the repeat chat the code came from; nil when it came from the first chat

	pending    bool         // a login is owed: set by Start, cleared by Complete / ClearPendingIf
	attempt    uint64       // incremented by every successful Start, and by an auth failure StartAuto refuses
	lastStart  uint64       // the attempt number the last successful Start took (StartAfterUserFailure)
	onComplete []func()     // run (outside mu) each time pending clears
	link       *linkMessage // the sign-in link message of this pending login (link.go); cleared with pending
}

// G is the process-wide re-login gate.
var G = &Gate{}

// Start attempts to claim the gate for a re-login. Returns true if claimed (the
// caller should run the driver); false if a re-login is already in progress.
// It is the manual trigger (/login): it starts an attempt whether or not a
// login is already pending. Automatic triggers use StartAuto.
func (g *Gate) Start() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.startLocked()
}

// StartAuto is Start for an automatic trigger: an auth failure (401) or the
// startup readiness probe. An expiry gets one automatic attempt (#3006): while
// a login is pending (an attempt failed and nothing has worked since), it
// starts nothing. Each attempt sends a new sign-in link, so without this every
// 401 from the dead credential started another one. Only a manual /login
// (Start) or a user-typed message that met the failure
// (StartAfterUserFailure, #3015) tries again. A refused trigger still advances
// the attempt number: the failure shows the credential is still dead, so a
// user turn that met it must not clear the pending state (ClearPendingIf).
func (g *Gate) StartAuto() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pending && !g.active {
		g.attempt++
		return false
	}
	return g.startLocked()
}

// StartAfterUserFailure starts one new attempt after a user-typed turn met an
// auth failure while a login was pending (#3015, Dick 2026-11-10: once the
// link has expired, the next user-typed message in any chat starts one new
// attempt, and its link goes to that chat). attempt is the number
// PendingAttempt returned before the turn ran. It starts an attempt only when
// a login is pending, none is running, an auth failure was refused during the
// turn (the attempt number moved), and no attempt started since the turn
// began (two chats' failed turns start one attempt between them, and a /login
// meanwhile is not followed by another). Background and system work never
// calls it: their 401s stay refused by StartAuto.
func (g *Gate) StartAfterUserFailure(attempt uint64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.pending || g.active || g.attempt == attempt || g.lastStart > attempt {
		return false
	}
	return g.startLocked()
}

// startLocked claims the gate; mu must be held.
func (g *Gate) startLocked() bool {
	if g.active {
		return false
	}
	g.active = true
	g.pending = true
	g.attempt++
	g.lastStart = g.attempt
	g.resetCaptureLocked()
	g.codeCh = make(chan string, 1)
	g.released = make(chan struct{})
	return true
}

// resetCaptureLocked closes the capture window and forgets its chats; mu must
// be held.
func (g *Gate) resetCaptureLocked() {
	g.captureOpen = false
	g.linkText = ""
	g.chats = nil
	g.codeChat = nil
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
// has started (and no auth failure was reported, see StartAuto) since attempt
// was read: a user turn ran to the end without a 401, so the credential works
// (for example, it was renewed outside foci).
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

// OpenCapture opens the code-capture window. agentID and sessionKey name the
// chat the sign-in link is sent to (an empty sessionKey: the chat is not
// known, so any chat of agentID counts as that chat), and linkText is the
// link message, which OfferLink repeats in any other chat (#3011).
func (g *Gate) OpenCapture(agentID, sessionKey, linkText string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.captureOpen = true
	g.linkText = linkText
	g.chats = map[chatRef]*linkChat{{agentID, sessionKey}: {shown: true}}
	g.codeChat = nil
}

// capturingChatLocked returns the chat (agentID, sessionKey) when a user
// message there is the login code: the capture window is open and that chat
// has the sign-in link. nil otherwise; mu must be held.
func (g *Gate) capturingChatLocked(agentID, sessionKey string) *linkChat {
	c := g.chatLocked(agentID, sessionKey)
	if !g.active || !g.captureOpen || c == nil || !c.shown {
		return nil
	}
	return c
}

// CaptureCode takes text as the login code when the chat (agentID,
// sessionKey) has the sign-in link and the capture window is open, and
// reports whether it did. The code from any chat that has the link completes
// the login (#3011).
func (g *Gate) CaptureCode(agentID, sessionKey, text string) bool {
	g.mu.Lock()
	c := g.capturingChatLocked(agentID, sessionKey)
	if c == nil {
		g.mu.Unlock()
		return false
	}
	if c.target != nil {
		g.codeChat = c
	}
	g.mu.Unlock()
	g.SubmitCode(text)
	return true
}

// SubmitCode delivers a captured login code to the waiting driver and closes
// the capture window. Non-blocking: a second submission while one is buffered
// (or with no driver waiting) is dropped.
func (g *Gate) SubmitCode(code string) {
	g.mu.Lock()
	ch := g.codeCh
	g.captureOpen = false
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
	g.resetCaptureLocked()
	g.codeCh = nil
	if g.released != nil {
		close(g.released)
		g.released = nil
	}
}
