package tools

// AsyncNotifier delivers async tool results to the agent session.
// Tools call Notify() with the originating session key and a formatted
// message; routing is handled centrally instead of per-tool.
type AsyncNotifier struct {
	fn func(sessionKey, message string)
}

// NewAsyncNotifier creates an AsyncNotifier that calls fn with each message.
// The sessionKey identifies which session originated the command.
func NewAsyncNotifier(fn func(sessionKey, message string)) *AsyncNotifier {
	return &AsyncNotifier{fn: fn}
}

// Notify delivers a message to the specified agent session. Safe to call
// on a nil receiver or with a nil fn.
func (n *AsyncNotifier) Notify(sessionKey, message string) {
	if n != nil && n.fn != nil {
		n.fn(sessionKey, message)
	}
}
