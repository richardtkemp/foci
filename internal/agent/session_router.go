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
	// warnf reports a refused self-registration (see Register). Optional;
	// nil in unit tests that build the router directly.
	warnf func(format string, args ...any)
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
	if r.routesTo(sink) {
		// Registering the router (or any decoration of it) as its own current
		// sink makes Emit recurse until the stack overflows — this took the
		// gateway down on 2026-09-19 when the tracing wrapper around a platform
		// turn's ctx sink (the router) slipped past the orchestrator's identity
		// guard (#1944). The existing registration stays; nothing is lost,
		// because events through the wrapper reach the router anyway.
		if r.warnf != nil {
			r.warnf("sessionRouter: refused to register a sink that routes back to this router (%T) — would recurse (#1944)", sink)
		}
		return
	}
	r.current.Store(&sinkRef{s: sink})
}

// routesTo reports whether emitting to sink would land on this router —
// i.e. sink is the router itself or a decorator chain (tracing, logging)
// whose innermost sink is the router. Callers deciding whether a ctx sink
// "already is the router" must use this rather than an identity compare, so
// a wrapped router is never registered into itself.
func (r *sessionRouter) routesTo(sink turnevent.Sink) bool {
	if sink == nil {
		return false
	}
	return turnevent.Unwrap(sink) == turnevent.Sink(r)
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

// DeliversToPlatform implements turnevent.Sink by forwarding the answer from
// whichever sink Emit would currently route to: the registered per-turn sink
// when one is set, or the fallback. Lets callers ask "if I emit now, does it
// reach a user-facing platform?" via the same dispatch shape as Emit.
func (r *sessionRouter) DeliversToPlatform() bool {
	if ref := r.current.Load(); ref != nil {
		return ref.s.DeliversToPlatform()
	}
	return r.fallback.DeliversToPlatform()
}
