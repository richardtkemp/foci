// dispatcher.go — per-Backend event dispatcher. One goroutine per
// registered Backend, started by Server.registerSession, stopped by
// Server.unregisterSession. Drains b.events (the channel Server.route
// pushes to) and invokes the Backend's handler callback per event,
// serially — preserving ccstream's "events for one session are
// sequential" invariant while letting different sessions progress in
// parallel.
//
// Step 4 scope: drain loop + start/stop. The handler itself is a
// pluggable field on Backend (dispatchHandler); Step 7 replaces the
// default no-op logger with the real per-Event-Type dispatch that fires
// SessionEvents / TurnEvents.

package opencode

import (
	"foci/internal/log"
)

// eventHandler is the per-Backend callback invoked for every event
// routed to the Backend. Implementations are called serially from the
// dispatch goroutine — no internal synchronisation needed.
type eventHandler func(ev rawEvent)

// defaultDispatchHandler is the Step 4 placeholder. Step 7 replaces
// this via Backend.SetDispatchHandler (or by direct field assignment
// in Backend.Start) with the real per-Event-Type dispatch that fires
// SessionEvents / TurnEvents.
//
// Logging at DEBUG gives operators visibility into event flow without
// the noise of ccstream's per-event reader logs.
var defaultDispatchHandler eventHandler = func(ev rawEvent) {
	log.Debugf("opencode", "dispatch: %s", ev.Type)
}

// dispatchLoop is the per-Backend drain goroutine. Reads from b.events
// and invokes handler. Stops when stopCh is closed (which happens in
// unregisterSession, or in Step 5's Backend.Close).
//
// The handler is read once at startup and bound for the loop's lifetime
// — changing Backend.dispatchHandler after startDispatcher has no effect
// until the next start. Step 7 sets the handler before calling
// registerSession; tests do the same.
func (b *Backend) dispatchLoop(handler eventHandler, stopCh <-chan struct{}) {
	for {
		select {
		case ev := <-b.events:
			handler(ev)
		case <-stopCh:
			return
		}
	}
}

// startDispatcher launches the dispatch goroutine using the Backend's
// current dispatchHandler (defaultDispatchHandler if nil). Returns a
// stop function that closes the internal stopCh, signalling the
// goroutine to exit. Idempotent — calling startDispatcher twice starts
// two goroutines; production callers (registerSession) guard against
// that by checking b.stopDispatcher != nil.
func (b *Backend) startDispatcher() func() {
	handler := b.dispatchHandler
	if handler == nil {
		handler = defaultDispatchHandler
	}
	stopCh := make(chan struct{})
	b.dispatchWg.Add(1)
	go func() {
		defer b.dispatchWg.Done()
		b.dispatchLoop(handler, stopCh)
	}()
	return func() { close(stopCh) }
}

// SetDispatchHandler installs the per-event handler. Step 7 calls this
// from Backend.Start (after BeginTurn bookkeeping is initialised but
// before registerSession, so the handler is bound before any event can
// arrive). For Step 4 the default handler is in effect.
//
// Must be called BEFORE startDispatcher / registerSession — the handler
// is captured at goroutine-start time and changes mid-flight have no
// effect.
func (b *Backend) SetDispatchHandler(h eventHandler) {
	b.dispatchHandler = h
}
