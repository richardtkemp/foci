package telemetry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"strconv"
	"strings"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Every id is derived, never random: sha256 over a role prefix and the
// api.db turn id, so any writer that knows the turn id — the agent when the
// turn runs, the log hook when a row lands minutes later, a peer agent's
// injected turn — computes the same trace and parent without coordination.
// The one random fallback is for spans started without an explicit id
// (never expected; kept so a bug degrades to an unlinked span, not a panic).

// TraceIDForTurn is the trace every span of a turn belongs to.
func TraceIDForTurn(turnID string) trace.TraceID {
	var id trace.TraceID
	copy(id[:], digest("foci:turn:", turnID)[:16])
	return id
}

// RootSpanID is the turn's root span — parent of its tool calls, subagent
// runs and its own generation rows.
func RootSpanID(turnID string) trace.SpanID { return spanID("foci:root:", turnID) }

// ToolSpanID is the span of one tool call within a turn.
func ToolSpanID(turnID, toolUseID string) trace.SpanID {
	return spanID("foci:tool:", turnID+":"+toolUseID)
}

// SubagentSpanID is the span of one CC subagent RUN. Run 1 — the Agent tool
// spawn — has no run suffix, so log rows (which carry the tool_use id as
// subagent_id — #1946) can parent onto it without knowing the run count;
// SendMessage reactivations get their own spans keyed by run index.
func SubagentSpanID(turnID, groupKey string, run int) trace.SpanID {
	if run <= 1 {
		return spanID("foci:subagent:", turnID+":"+groupKey)
	}
	return spanID("foci:subagent:", turnID+":"+groupKey+":"+strconv.Itoa(run))
}

// GenerationSpanID names one api.db row's observation. The parts are the
// row's discriminators (turn id, call type, agent id, model, timestamp); two
// instalments of one accumulated subagent row differ in timestamp.
func GenerationSpanID(parts ...string) trace.SpanID {
	return spanID("foci:gen:", strings.Join(parts, "\x00"))
}

// EventSpanID names a point-in-time child (system prompt, cost correction).
func EventSpanID(parts ...string) trace.SpanID {
	return spanID("foci:event:", strings.Join(parts, "\x00"))
}

// RowTraceID is the trace for an api.db row that belongs to no turn
// (compaction, summariser, spawn rows written without a turn id).
func RowTraceID(session, callType string, ts time.Time) trace.TraceID {
	var id trace.TraceID
	copy(id[:], digest("foci:row:", session+"\x00"+callType+"\x00"+strconv.FormatInt(ts.UnixNano(), 10))[:16])
	return id
}

func digest(prefix, s string) []byte {
	h := sha256.Sum256([]byte(prefix + s))
	return h[:]
}

func spanID(prefix, s string) trace.SpanID {
	var id trace.SpanID
	copy(id[:], digest(prefix, s)[:8])
	return id
}

// idGenerator hands the SDK the ids the caller staged on the context, so a
// span's identity is a pure function of what it represents.
type idGenerator struct{}

type wantIDs struct {
	traceID trace.TraceID
	spanID  trace.SpanID
}

type wantIDsKey struct{}

// withIDs stages the ids the next Start on ctx must use. A zero traceID means
// "inherit from the parent span context on ctx" (the SDK only calls NewIDs
// when there is no parent, so a child with a staged spanID goes through
// NewSpanID).
func withIDs(ctx context.Context, traceID trace.TraceID, id trace.SpanID) context.Context {
	return context.WithValue(ctx, wantIDsKey{}, wantIDs{traceID: traceID, spanID: id})
}

func (idGenerator) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	if w, ok := ctx.Value(wantIDsKey{}).(wantIDs); ok && w.traceID.IsValid() && w.spanID.IsValid() {
		return w.traceID, w.spanID
	}
	var t trace.TraceID
	var s trace.SpanID
	_, _ = rand.Read(t[:])
	_, _ = rand.Read(s[:])
	return t, s
}

func (idGenerator) NewSpanID(ctx context.Context, _ trace.TraceID) trace.SpanID {
	if w, ok := ctx.Value(wantIDsKey{}).(wantIDs); ok && w.spanID.IsValid() {
		return w.spanID
	}
	var s trace.SpanID
	_, _ = rand.Read(s[:])
	return s
}

var _ sdktrace.IDGenerator = idGenerator{}

// parentContext returns a context whose current span is the (possibly
// not-yet-exported, possibly long-finished) span with the given ids, so a
// child can be started against it. Remote=true tells the SDK not to expect
// the parent in-process.
func parentContext(ctx context.Context, traceID trace.TraceID, parent trace.SpanID) context.Context {
	return trace.ContextWithRemoteSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     parent,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	}))
}
