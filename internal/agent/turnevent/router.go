package turnevent

import (
	"context"
	"sync/atomic"
)

// SessionRouter is a session-scoped dispatch layer between a backend's
// event stream and the per-turn rendering target.
//
// The router lives for the session's lifetime. Per-turn callers (typically
// Bot.Drive) Register a Sink at turn start and Clear it at turn end. The
// backend invokes Emit unconditionally; the router decides where the event
// goes:
//
//   - Current per-turn Sink set: forward to it (normal in-turn delivery,
//     e.g. turn.StreamingSink driving a renderer).
//   - No current Sink: forward to the fallback (late-delivery path, e.g.
//     a turn.SessionSink that sends a fresh standalone message to the
//     session via platform.Connection.SendToSession).
//
// This decouples delivery routing (session lifetime) from per-turn UI
// rendering (turn lifetime). Late text emitted by the backend after a turn
// handler has fired its OnTurnComplete — for example stacked queued events
// under ccstream's rearm-counter mechanism — routes correctly because the
// router outlives any single turn's per-turn sink.
//
// SessionRouter is safe for concurrent Emit / Register / Clear. The current
// per-turn slot uses atomic.Pointer; the fallback is immutable after
// construction.
type SessionRouter struct {
	fallback Sink
	current  atomic.Pointer[sinkRef]
}

// sinkRef wraps a Sink so atomic.Pointer can store a single pointer per
// registration. Storing a pointer to the interface value directly is also
// possible but requires an explicit heap allocation; this wrapper makes the
// allocation intentional and the type explicit.
type sinkRef struct{ s Sink }

// NewSessionRouter constructs a router with the given fallback sink.
// fallback is invoked whenever no per-turn sink is registered. A nil
// fallback is replaced with NopSink, so callers can disable late delivery
// by passing nil rather than constructing a NopSink themselves.
func NewSessionRouter(fallback Sink) *SessionRouter {
	if fallback == nil {
		fallback = nopSinkSingleton
	}
	return &SessionRouter{fallback: fallback}
}

// Register installs sink as the current per-turn target. Subsequent Emit
// calls forward to it until Clear is called or another Register replaces
// it. Passing a nil sink is equivalent to Clear.
//
// Per-turn callers should pair Register with Clear in a defer block:
//
//	router.Register(streamingSink)
//	defer router.Clear()
func (r *SessionRouter) Register(sink Sink) {
	if sink == nil {
		r.current.Store(nil)
		return
	}
	r.current.Store(&sinkRef{s: sink})
}

// Clear removes the current per-turn sink. Subsequent Emit calls fall back
// to the fallback sink. Idempotent — safe to call when no sink is
// registered.
func (r *SessionRouter) Clear() {
	r.current.Store(nil)
}

// Emit implements Sink. Dispatches to the current per-turn sink if one is
// registered; otherwise forwards to the fallback. Safe for concurrent use.
func (r *SessionRouter) Emit(ctx context.Context, ev Event) {
	if ref := r.current.Load(); ref != nil {
		ref.s.Emit(ctx, ev)
		return
	}
	r.fallback.Emit(ctx, ev)
}
