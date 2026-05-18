package agent

import "context"

// Per-turn progress signal: a one-shot callback fired after the transport
// has written the primary user message to the backend. The inbox's session
// worker installs this so `inb.turnActive` flips to true only AFTER the
// backend has received the primary — closing the reorder race where a fast
// follow-up could be Inject()'d to ccstream's stdin before the primary's
// own Inject call completed (see clutch/docs/inbox-steer-reorder-bug.md,
// Option A; TODO #777).
//
// The producer (RunInference, after a successful Inject / Send) calls the
// returned func. The consumer (sessionWorker) wraps its closure in
// sync.Once so multiple fires across follow-up turns or transport retries
// are idempotent. Off-inbox call sites (HTTP /send async, cross-agent
// dispatch, reflection) leave the callback unset; OnPrimaryWrittenFromContext
// returns a nop and they pay nothing.

type onPrimaryWrittenKey struct{}

// WithOnPrimaryWritten returns ctx with fn registered to fire once the
// transport's primary message has reached the backend. Pass through to
// RunInference via the standard ctx flow; the transport invokes the
// callback after its first successful write.
func WithOnPrimaryWritten(ctx context.Context, fn func()) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, onPrimaryWrittenKey{}, fn)
}

// OnPrimaryWrittenFromContext returns the callback registered via
// WithOnPrimaryWritten, or a nop func if none was set. Callers can always
// invoke the result safely without nil checks.
func OnPrimaryWrittenFromContext(ctx context.Context) func() {
	if fn, ok := ctx.Value(onPrimaryWrittenKey{}).(func()); ok && fn != nil {
		return fn
	}
	return func() {}
}
