package tools

import (
	"fmt"
	"sync"
	"time"

	"foci/internal/clock"
)

// AsyncNotifier delivers async tool results to the agent session.
// Tools call Notify() with the originating session key and a formatted
// message; routing is handled centrally instead of per-tool.
//
// It also tracks pending async results per session so that compaction
// can be deferred until all outstanding results have been delivered.
type AsyncNotifier struct {
	fn func(targetSession, message, replyToSession, trigger string, origin *AgentOrigin)

	mu sync.Mutex
	// pending holds, per session key, the start time of each pending result,
	// oldest first. Its length is the pending count; the times feed
	// YoungestPending (#3112).
	pending map[string][]time.Time
	// onChange is called with the session key after its pending set changed
	// (SetOnChange); nil = none.
	onChange func(sessionKey string)

	// clk arms RunInBackground's auto-background threshold timer. nil means
	// the wall clock; tests set a *clock.Fake through WithClock.
	clk clock.Clock

	// thenFn delivers like fn and then reports whether the target's turn
	// took the message (InjectToAgentThen). nil = fn, reported as taken.
	thenFn DeliverThenFunc

	// checkFn reports a send_to_session target's state at send time
	// (CheckTarget). nil = every target reads as available.
	checkFn TargetCheckFunc
}

// TargetCheckFunc reports whether a target session can take a message now.
// err means the target does not exist, so nothing can be delivered. A non-empty
// reason means it exists but cannot run a turn now, and the message waits.
type TargetCheckFunc func(targetSession string) (reason string, err error)

// WithTargetCheck sets the check CheckTarget uses, and returns n. Set it before
// the notifier is shared; it is not synchronised.
func (n *AsyncNotifier) WithTargetCheck(fn TargetCheckFunc) *AsyncNotifier {
	n.checkFn = fn
	return n
}

// CheckTarget reports targetSession's state (TargetCheckFunc). Safe on a nil
// receiver and with no check set: the target reads as available.
func (n *AsyncNotifier) CheckTarget(targetSession string) (reason string, err error) {
	if n == nil || n.checkFn == nil {
		return "", nil
	}
	return n.checkFn(targetSession)
}

// UnavailableHeldNote is what a caller is told when target cannot run a turn
// now and its message waits (#2892, Dick 2026-11-06: tell the caller at once,
// still deliver the message, and say so). replyToCaller says where the reply
// goes once the target is back. The tool result and the later notice share it.
func UnavailableHeldNote(target, reason string, replyToCaller bool) string {
	where := "its own chat"
	if replyToCaller {
		where = "you"
	}
	return fmt.Sprintf("Session %s cannot reply now: %s. Your message is kept and will be delivered when it is back. The reply then goes to %s.", target, reason, where)
}

// DeliverThenFunc delivers message to targetSession as an injected turn, like
// the notifier's own fn, then calls done(true) once the turn has run, or
// done(false) when the inbox refused it (a shutdown drain, a full inbox).
// done is called at most once; never when the process ends first.
type DeliverThenFunc func(targetSession, message, trigger string, done func(delivered bool))

// WithDeliverThen sets the delivery InjectToAgentThen uses, and returns n. Set
// it before the notifier is shared; it is not synchronised.
func (n *AsyncNotifier) WithDeliverThen(fn DeliverThenFunc) *AsyncNotifier {
	n.thenFn = fn
	return n
}

// InjectToAgentThen is InjectToAgent for a caller holding a durable record of
// the message (a spawn's persisted result, #2589): done reports whether the
// target's turn took it. Without a WithDeliverThen delivery it injects through
// fn and reports true at once. Safe on a nil receiver (done is not called).
func (n *AsyncNotifier) InjectToAgentThen(targetSession, message, trigger string, done func(delivered bool)) {
	if n == nil {
		return
	}
	if done == nil {
		done = func(bool) {}
	}
	if n.thenFn != nil {
		n.thenFn(targetSession, message, trigger, done)
		return
	}
	n.inject(targetSession, message, "", trigger, nil)
	done(true)
}

// WithClock makes RunInBackground arm its auto-background threshold on c
// instead of the wall clock, and returns n. The notifier is the one object
// every auto-backgrounding tool (shell, http_request) already receives, so it
// carries the seam: a test drives the threshold with clock.Fake.Advance
// instead of waiting it out. Production never calls this. Set it before the
// notifier is shared; it is not synchronised.
func (n *AsyncNotifier) WithClock(c clock.Clock) *AsyncNotifier {
	n.clk = c
	return n
}

// clock returns the clock the threshold timer runs on (the wall clock unless
// WithClock set one).
func (n *AsyncNotifier) clock() clock.Clock {
	if n == nil || n.clk == nil {
		return clock.Real()
	}
	return n.clk
}

// NewAsyncNotifier creates an AsyncNotifier that calls fn with each message.
// The targetSession identifies which session should process the message.
// If replyToSession is non-empty, the response is routed to that session
// instead of being sent to targetSession's chat.
// The trigger identifies the source (e.g. "async_notify", "tmux_watch").
// origin is non-nil exactly when the message is an agent-to-agent message
// (send_to_session — see InjectAgentMessage); every other caller delivers
// with a nil origin.
func NewAsyncNotifier(fn func(targetSession, message, replyToSession, trigger string, origin *AgentOrigin)) *AsyncNotifier {
	return &AsyncNotifier{
		fn:      fn,
		pending: make(map[string][]time.Time),
	}
}

// SetOnChange registers fn to be called, outside the notifier's lock, with
// the session key whenever a session's pending results change (MarkPending,
// MarkDone). The app's waiting-on-other value re-reads YoungestPending then
// (#3112). Safe on a nil receiver.
func (n *AsyncNotifier) SetOnChange(fn func(sessionKey string)) {
	if n == nil {
		return
	}
	n.mu.Lock()
	n.onChange = fn
	n.mu.Unlock()
}

// YoungestPending returns the start time of the session's most recently
// started pending result, and false when none is pending. Safe on a nil
// receiver.
func (n *AsyncNotifier) YoungestPending(sessionKey string) (time.Time, bool) {
	if n == nil {
		return time.Time{}, false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	list := n.pending[sessionKey]
	if len(list) == 0 {
		return time.Time{}, false
	}
	return list[len(list)-1], true
}

// MarkPending increments the pending async result count for a session,
// recording the start time on the notifier's clock. Call this before
// dispatching async work. Safe to call on a nil receiver.
func (n *AsyncNotifier) MarkPending(sessionKey string) {
	if n == nil {
		return
	}
	now := n.clock().Now()
	n.mu.Lock()
	n.pending[sessionKey] = append(n.pending[sessionKey], now)
	fn := n.onChange
	n.mu.Unlock()
	if fn != nil {
		fn(sessionKey)
	}
}

// MarkDone decrements the pending async result count for a session. Results
// are not told apart, so the oldest start time goes: the youngest remaining
// start, which YoungestPending reports, can only be later than the truth.
// Safe to call on a nil receiver.
func (n *AsyncNotifier) MarkDone(sessionKey string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	changed := false
	if list := n.pending[sessionKey]; len(list) > 0 {
		changed = true
		if len(list) == 1 {
			delete(n.pending, sessionKey)
		} else {
			n.pending[sessionKey] = list[1:]
		}
	}
	fn := n.onChange
	n.mu.Unlock()
	if changed && fn != nil {
		fn(sessionKey)
	}
}

// HasPending returns true if the session has any pending async results.
// Safe to call on a nil receiver (returns false).
func (n *AsyncNotifier) HasPending(sessionKey string) bool {
	if n == nil {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.pending[sessionKey]) > 0
}

// InjectToAgent delivers a message to the specified agent session for processing.
// If replyToSession is non-empty, the agent's response is routed to that session
// instead of being sent to targetSession's chat.
// The trigger identifies the source (e.g. "async_notify", "tmux_watch") for
// the [meta] via= header.
// Safe to call on a nil receiver or with a nil fn.
func (n *AsyncNotifier) InjectToAgent(targetSession, message, replyToSession, trigger string) {
	n.inject(targetSession, message, replyToSession, trigger, nil)
}

// InjectAgentMessage delivers an agent-to-agent message (send_to_session with
// reply_to=caller, including its oneshot downgrade to fire-and-forget): same
// delivery as InjectToAgent under the shared "async_notify" trigger, plus the
// origin that lets the receiving chat surface what prompted the turn as a
// chit. replyToSession follows the tool's downgrade rules ("" = deliver the
// reply to the target's own chat). Safe to call on a nil receiver or with a
// nil fn.
func (n *AsyncNotifier) InjectAgentMessage(targetSession, message, replyToSession string, origin AgentOrigin) {
	n.inject(targetSession, message, replyToSession, "async_notify", &origin)
}

func (n *AsyncNotifier) inject(targetSession, message, replyToSession, trigger string, origin *AgentOrigin) {
	if n == nil || n.fn == nil {
		return
	}
	n.fn(targetSession, message, replyToSession, trigger, origin)
}
