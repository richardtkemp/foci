package agent

import (
	"context"
	"sync/atomic"

	"foci/internal/agent/turnevent"
)

// sessionRouter is a session-scoped dispatch layer between a backend's
// event stream and the per-turn rendering target.
//
// The router lives for the session's lifetime, lazy-built once per
// session by sessionRouterFor. Per-turn callers (Agent.RunTurn) register
// a sink at turn start and clear it at turn end. The backend invokes
// Emit unconditionally; the router decides where the event goes:
//
//   - Current per-turn Sink set: forward to it (normal in-turn delivery,
//     e.g. turn.StreamingSink driving a renderer).
//   - No current Sink: forward to the fallback (late-delivery path, e.g.
//     a turn.SessionSink that sends a fresh standalone message via
//     platform.Connection.SendToSession).
//
// This decouples delivery routing (session lifetime) from per-turn UI
// rendering (turn lifetime). Late text emitted by the backend after a
// turn handler's OnTurnComplete — e.g. stacked queued events under
// ccstream's rearm-counter mechanism — routes correctly because the
// router outlives any single turn's per-turn sink.
//
// sessionRouter is safe for concurrent Emit / Register / Clear via
// atomic.Pointer. The fallback is immutable after construction.
//
// Was originally turnevent.SessionRouter (TODO #745); consolidated to
// the agent package as unexported in TODO #746 Stage E since the agent
// is the only consumer.
type sessionRouter struct {
	fallback turnevent.Sink
	current  atomic.Pointer[sinkRef]
}

// sinkRef wraps a Sink so atomic.Pointer stores a single pointer per
// registration. Explicit wrapper type makes the allocation intentional.
type sinkRef struct{ s turnevent.Sink }

// newSessionRouter constructs a router with the given fallback sink.
// fallback is invoked whenever no per-turn sink is registered. A nil
// fallback substitutes a no-op singleton, so callers can disable late
// delivery by passing nil.
func newSessionRouter(fallback turnevent.Sink) *sessionRouter {
	if fallback == nil {
		fallback = turnevent.NopSink{}
	}
	return &sessionRouter{fallback: fallback}
}

// Register installs sink as the current per-turn target. Subsequent
// Emit calls forward to it until Clear or another Register replaces it.
// Passing a nil sink is equivalent to Clear.
func (r *sessionRouter) Register(sink turnevent.Sink) {
	if sink == nil {
		r.current.Store(nil)
		return
	}
	r.current.Store(&sinkRef{s: sink})
}

// Clear removes the current per-turn sink. Subsequent Emit calls fall
// back to the fallback. Idempotent.
func (r *sessionRouter) Clear() {
	r.current.Store(nil)
}

// Emit implements turnevent.Sink. Dispatches to the current per-turn
// sink if one is registered; otherwise forwards to the fallback.
func (r *sessionRouter) Emit(ctx context.Context, ev turnevent.Event) {
	if ref := r.current.Load(); ref != nil {
		ref.s.Emit(ctx, ev)
		return
	}
	r.fallback.Emit(ctx, ev)
}
