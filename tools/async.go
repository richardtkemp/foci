package tools

// AsyncNotifier delivers async tool results to the agent session.
// Tools call Notify() with a formatted message; routing is handled
// centrally instead of per-tool.
type AsyncNotifier struct {
	fn func(message string)
}

// NewAsyncNotifier creates an AsyncNotifier that calls fn with each message.
func NewAsyncNotifier(fn func(message string)) *AsyncNotifier {
	return &AsyncNotifier{fn: fn}
}

// Notify delivers a message to the agent session. Safe to call on a nil
// receiver or with a nil fn.
func (n *AsyncNotifier) Notify(message string) {
	if n != nil && n.fn != nil {
		n.fn(message)
	}
}
