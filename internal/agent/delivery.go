package agent

import (
	"context"
	"fmt"

	"foci/internal/delegator"
)

// Delivery tracking, agent side (#2050). A backend implementing
// delegator.DeliveryTracker reports every input it has not yet consumed;
// DelegatedManager persists those (delivery_store.go), clears them when the
// backend confirms consumption, and hands back the ones a dead process never
// read. This file carries a message's identity from the platform to the
// backend write, and turns a handed-back input into a fresh turn.

// TriggerRedelivery labels the fresh turn that re-sends an input a backend
// never consumed. It is a control trigger: the lost message was already due to
// be processed, so it is not held behind a pending foci_ask.
const TriggerRedelivery = "redelivery"

// inputRefsKey is the context key for the platform messages a turn carries.
type inputRefsKey struct{}

// WithInputRefs attaches the platform message refs a turn's input carries, so
// the backend write can report their consumption (the app's ✓✓).
func WithInputRefs(ctx context.Context, refs []delegator.InputRef) context.Context {
	if len(refs) == 0 {
		return ctx
	}
	return context.WithValue(ctx, inputRefsKey{}, refs)
}

// InputRefsFromContext returns the refs set by WithInputRefs (nil if none).
func InputRefsFromContext(ctx context.Context) []delegator.InputRef {
	r, _ := ctx.Value(inputRefsKey{}).([]delegator.InputRef)
	return r
}

// batchInputRefs collects the refs of every envelope in a batch.
func batchInputRefs(batch []Envelope) []delegator.InputRef {
	var refs []delegator.InputRef
	for _, env := range batch {
		if env.Ref.MessageID != "" {
			refs = append(refs, env.Ref)
		}
	}
	return refs
}

// redeliveryKey is the context key for the input a redelivery turn re-sends.
type redeliveryKey struct{}

// Redelivery is an input re-sent as a fresh turn, with why it was re-sent.
type Redelivery struct {
	Input delegator.PendingInput
	// Reason is "cc-exit" (the process died holding it) or "restart" (foci
	// restarted before the process confirmed it).
	Reason string
}

// WithRedelivery marks ctx's turn as re-sending r.Input. The turn transport
// then writes the input verbatim — it already carries its own [meta] header,
// nudges and orientation from when it was first composed — under its original
// delivery id, so the message keeps one identity end to end.
func WithRedelivery(ctx context.Context, r Redelivery) context.Context {
	return context.WithValue(ctx, redeliveryKey{}, &r)
}

// redeliveryFromContext returns the redelivery set by WithRedelivery, or nil.
func redeliveryFromContext(ctx context.Context) *Redelivery {
	r, _ := ctx.Value(redeliveryKey{}).(*Redelivery)
	return r
}

// redeliveryPrompt is the text a redelivery turn writes: a one-line tag, so the
// agent knows it may have seen part of this before, then the original input.
func redeliveryPrompt(r *Redelivery) string {
	return fmt.Sprintf("[meta] redelivered=%s — foci re-sent this message because the previous backend process never read it\n\n%s",
		r.Reason, r.Input.Text)
}
