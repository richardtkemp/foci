package turnevent

// Steerer is the pull-direction counterpart to Sink: the agent queries it at
// safe points during a turn to pick up any queued user messages and fold them
// into the next prompt.
//
// This is intentionally separate from the event stream because it is pull,
// not push: the agent needs a return value. Sinks emit; Steerers answer.
type Steerer interface {
	// PendingSteers returns any queued steer messages and drains them.
	// Returns nil (or empty slice) when nothing is pending. Must be
	// non-blocking — the agent calls this in hot loops.
	PendingSteers() []string
}

// SteererFunc adapts a plain function to the Steerer interface.
type SteererFunc func() []string

// PendingSteers implements Steerer.
func (f SteererFunc) PendingSteers() []string { return f() }
