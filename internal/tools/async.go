package tools

import "sync"

// AsyncNotifier delivers async tool results to the agent session.
// Tools call Notify() with the originating session key and a formatted
// message; routing is handled centrally instead of per-tool.
//
// It also tracks pending async results per session so that compaction
// can be deferred until all outstanding results have been delivered.
type AsyncNotifier struct {
	fn func(targetSession, message, replyToSession, trigger string)

	mu      sync.Mutex
	pending map[string]int // session key → count of pending results
}

// NewAsyncNotifier creates an AsyncNotifier that calls fn with each message.
// The targetSession identifies which session should process the message.
// If replyToSession is non-empty, the response is routed to that session
// instead of being sent to targetSession's chat.
// The trigger identifies the source (e.g. "async_notify", "tmux_watch").
func NewAsyncNotifier(fn func(targetSession, message, replyToSession, trigger string)) *AsyncNotifier {
	return &AsyncNotifier{
		fn:      fn,
		pending: make(map[string]int),
	}
}

// MarkPending increments the pending async result count for a session.
// Call this before dispatching async work. Safe to call on a nil receiver.
func (n *AsyncNotifier) MarkPending(sessionKey string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	n.pending[sessionKey]++
	n.mu.Unlock()
}

// MarkDone decrements the pending async result count for a session.
// Safe to call on a nil receiver.
func (n *AsyncNotifier) MarkDone(sessionKey string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	if n.pending[sessionKey] > 0 {
		n.pending[sessionKey]--
		if n.pending[sessionKey] == 0 {
			delete(n.pending, sessionKey)
		}
	}
	n.mu.Unlock()
}

// HasPending returns true if the session has any pending async results.
// Safe to call on a nil receiver (returns false).
func (n *AsyncNotifier) HasPending(sessionKey string) bool {
	if n == nil {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.pending[sessionKey] > 0
}

// InjectToAgent delivers a message to the specified agent session for processing.
// If replyToSession is non-empty, the agent's response is routed to that session
// instead of being sent to targetSession's chat.
// The trigger identifies the source (e.g. "async_notify", "tmux_watch") for
// the [meta] via= header.
// Safe to call on a nil receiver or with a nil fn.
func (n *AsyncNotifier) InjectToAgent(targetSession, message, replyToSession, trigger string) {
	if n == nil || n.fn == nil {
		return
	}
	n.fn(targetSession, message, replyToSession, trigger)
}
