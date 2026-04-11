package turnevent

import "context"

type sinkKey struct{}
type steererKey struct{}

// WithSink attaches a Sink to ctx. The agent reads it via SinkFromContext
// during the turn and emits events through it.
func WithSink(ctx context.Context, sink Sink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, sinkKey{}, sink)
}

// SinkFromContext returns the Sink attached to ctx, or a NopSink if none is
// set. Producers can call Emit unconditionally without nil-checking.
func SinkFromContext(ctx context.Context) Sink {
	if s, ok := ctx.Value(sinkKey{}).(Sink); ok {
		return s
	}
	return nopSinkSingleton
}

// Emit is a convenience for SinkFromContext(ctx).Emit(ctx, ev).
func Emit(ctx context.Context, ev Event) {
	SinkFromContext(ctx).Emit(ctx, ev)
}

// WithSteerer attaches a Steerer to ctx. The agent polls it at safe points
// during the turn to fold pending user input into the next prompt.
func WithSteerer(ctx context.Context, s Steerer) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, steererKey{}, s)
}

// SteererFromContext returns the Steerer attached to ctx, or nil if none is set.
// Callers should nil-check; a missing Steerer means the turn does not support
// steering (e.g. HTTP handlers, hook-driven internal turns).
func SteererFromContext(ctx context.Context) Steerer {
	if s, ok := ctx.Value(steererKey{}).(Steerer); ok {
		return s
	}
	return nil
}
