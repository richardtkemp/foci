package tools

import "sync"

// AsyncNotifier delivers async tool results to the agent session.
// Tools call Notify() with the originating session key and a formatted
// message; routing is handled centrally instead of per-tool.
//
// It also tracks pending async results per session so that compaction
// can be deferred until all outstanding results have been delivered.
//
// When a session key is rotated (e.g. compaction), MigrateSession remaps
// the old key so that in-flight async goroutines — which captured the old
// key at dispatch time — resolve to the new key when they deliver results.
type AsyncNotifier struct {
	fn func(targetSession, message, replyToSession, trigger string)

	mu      sync.Mutex
	pending map[string]int    // session key → count of pending results
	remaps  map[string]string // old session key → new session key
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
// Resolves through key remaps so that async goroutines holding a
// pre-rotation key correctly decrement the current key's count.
// Safe to call on a nil receiver.
func (n *AsyncNotifier) MarkDone(sessionKey string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	resolved := n.resolveKey(sessionKey)
	if n.pending[resolved] > 0 {
		n.pending[resolved]--
		if n.pending[resolved] == 0 {
			delete(n.pending, resolved)
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
// Resolves through key remaps so that async goroutines holding a pre-rotation
// key deliver to the current (rotated) session.
// If replyToSession is non-empty, the agent's response is routed to that session
// instead of being sent to targetSession's chat.
// The trigger identifies the source (e.g. "async_notify", "tmux_watch") for
// the [meta] via= header.
// Safe to call on a nil receiver or with a nil fn.
func (n *AsyncNotifier) InjectToAgent(targetSession, message, replyToSession, trigger string) {
	if n == nil || n.fn == nil {
		return
	}
	n.mu.Lock()
	resolved := n.resolveKey(targetSession)
	n.mu.Unlock()
	n.fn(resolved, message, replyToSession, trigger)
}

// MigrateSession remaps an old session key to a new one. In-flight async
// goroutines that captured the old key will resolve to the new key when
// they call InjectToAgent or MarkDone. The pending count is also moved
// from the old key to the new key. Safe to call on a nil receiver.
func (n *AsyncNotifier) MigrateSession(oldKey, newKey string) {
	if n == nil || oldKey == newKey || newKey == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()

	// Move pending count
	if count := n.pending[oldKey]; count > 0 {
		n.pending[newKey] += count
		delete(n.pending, oldKey)
	}

	// Add remap
	if n.remaps == nil {
		n.remaps = make(map[string]string)
	}
	n.remaps[oldKey] = newKey

	// Update any existing remaps that pointed to oldKey (chain flattening).
	// This handles multi-rotation: if A→B existed and now B→C, update A→C.
	for k, v := range n.remaps {
		if v == oldKey {
			n.remaps[k] = newKey
		}
	}
}

// resolveKey follows the remap chain to find the current key.
// Must be called with n.mu held.
func (n *AsyncNotifier) resolveKey(key string) string {
	if n.remaps == nil {
		return key
	}
	if newKey, ok := n.remaps[key]; ok {
		return newKey
	}
	return key
}
