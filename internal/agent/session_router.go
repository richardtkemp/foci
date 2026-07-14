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

	// lateStream is an optional streaming sink consulted ONLY when no per-turn
	// sink is registered — i.e. during a CC autonomous run foci opened no turn
	// for (#1070/#1257). It lets autonomous output stream to a platform (the app
	// appSink: text.delta + activity frames) instead of falling to the fallback's
	// whole-message SessionSink. Deliberately a SEPARATE slot from current: the
	// autonomous-adoption lifecycle sets/clears it independently of per-turn
	// Register/Clear, so it can never race with — or clobber — a real turn's sink.
	// nil (the default) preserves the original current→fallback dispatch.
	lateStream atomic.Pointer[sinkRef]
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

// SetLateStream installs sink as the late-delivery streaming target used when no
// per-turn sink is registered (a CC autonomous run). Passing nil clears it.
// Independent of Register/Clear so the autonomous-adoption lifecycle owns it.
func (r *sessionRouter) SetLateStream(sink turnevent.Sink) {
	if sink == nil {
		r.lateStream.Store(nil)
		return
	}
	r.lateStream.Store(&sinkRef{s: sink})
}

// ClearLateStream removes the late-delivery streaming sink. Subsequent
// no-per-turn-sink Emit calls fall back to the fallback again. Idempotent.
func (r *sessionRouter) ClearLateStream() {
	r.lateStream.Store(nil)
}

// Emit implements turnevent.Sink. Dispatches to the current per-turn
// sink if one is registered; otherwise forwards to the fallback.
func (r *sessionRouter) Emit(ctx context.Context, ev turnevent.Event) {
	if ref := r.current.Load(); ref != nil {
		ref.s.Emit(ctx, ev)
		return
	}
	if ref := r.lateStream.Load(); ref != nil {
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
	if ref := r.lateStream.Load(); ref != nil {
		return ref.s.DeliversToPlatform()
	}
	return r.fallback.DeliversToPlatform()
}
