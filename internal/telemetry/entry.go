package telemetry

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"foci/internal/delegator/accounting"
	"foci/internal/modelinfo"
	"foci/internal/session"
)

// recordBooking is accounting.BookedHook: one booked call → one "generation"
// observation.
//
// Exactly one, and the only place cost is billed, is the invariant that makes
// Langfuse's daily totals equal the ledger's. The root/tool/subagent spans
// carry cost only as read-only metadata. Parentage is derived from the call
// alone: a subagent's call hangs under its subagent's span (both are keyed by
// the Agent tool_use id), a conversation turn's call under that turn's root,
// and a helper call booked on a turn of its own (compaction, summary, spawn)
// becomes its own one-observation trace named after its call type.
//
// b.Instalment is true when a legacy subagent share was FOLDED into an
// already-booked call rather than inserted (#1922): the ledger holds one call
// per delegation, but an append-only sink cannot update, so each instalment
// is its own observation and the delegation's cost is their sum — same
// arithmetic as api.jsonl, which also receives every instalment.
func recordBooking(b accounting.Booking) {
	tr, o, _, ok := current()
	if !ok {
		return
	}
	start := b.BilledAt
	if start.IsZero() {
		start = time.Now()
	}
	durationMS := detailInt(b.Detail, "duration_ms")
	end := start.Add(time.Duration(durationMS) * time.Millisecond)
	if end.Before(start) {
		end = start
	}
	agent := b.AgentID

	// Token scope: a call's own counts, or — on a legacy row — the turn totals
	// its figure was priced from, or before #1854 the final cycle's fill.
	scope := "call"
	if b.Kind == accounting.KindLegacy {
		scope = "snapshot"
		if t, _ := b.Detail["turn_totals"].(bool); t {
			scope = "turn"
		}
	}
	in, out := b.Tokens[modelinfo.ClassInput], b.Tokens[modelinfo.ClassOutput]+b.Tokens[modelinfo.ClassReasoning]
	cr := b.Tokens[modelinfo.ClassCacheRead]
	cw := b.Tokens[modelinfo.ClassCacheWrite5m] + b.Tokens[modelinfo.ClassCacheWrite1h] + b.Tokens[modelinfo.ClassCacheWrite]
	usage := map[string]int{
		"input":                       in,
		"output":                      out,
		"cache_read_input_tokens":     cr,
		"cache_creation_input_tokens": cw,
		"total":                       in + out + cr + cw,
	}
	usageB, _ := json.Marshal(usage)
	cost := 0.0
	if b.CostUSD != nil {
		cost = *b.CostUSD
	}
	costB, _ := json.Marshal(map[string]float64{"total": cost})

	callType := callTypeOf(b)
	attrs := []attribute.KeyValue{
		attribute.String(attrObsType, "generation"),
		attribute.String(attrObsUsage, string(usageB)),
		attribute.String(attrObsCost, string(costB)),
		attribute.String(attrObsLevel, "DEFAULT"),
		attribute.String(attrUserID, agent),
		attribute.String(attrSessionID, b.Session),
		attribute.String(attrEnvironment, o.Environment),
		attribute.String(attrObsMetaPrefix+"source", "foci"),
		attribute.String(attrObsMetaPrefix+"call_type", callType),
		attribute.String(attrObsMetaPrefix+"token_scope", scope),
		attribute.String(attrObsMetaPrefix+"model_raw", b.Model),
		attribute.String(attrObsMetaPrefix+"provider", b.Provider),
		attribute.String(attrObsMetaPrefix+"backend", b.Backend),
		attribute.String(attrObsMetaPrefix+"stop_reason", b.StopReason),
		attribute.Int64(attrObsMetaPrefix+"duration_ms", durationMS),
		attribute.Int(attrObsMetaPrefix+"context_tokens", b.Fill),
	}
	if m := modelinfo.Normalize(b.Model); m != "" && b.Model != "<synthetic>" {
		attrs = append(attrs, attribute.String(attrObsModel, m))
	}
	if b.CostUSD == nil {
		attrs = append(attrs, attribute.Bool(attrObsMetaPrefix+"unpriced", true))
	}
	if b.Key != "" {
		attrs = append(attrs, attribute.String(attrObsMetaPrefix+"call_key", b.Key))
	}
	if b.TurnID != "" {
		attrs = append(attrs, attribute.String(attrObsMetaPrefix+"turn_id", b.TurnID))
	}
	if b.Actor != "" {
		attrs = append(attrs, attribute.String(attrObsMetaPrefix+"subagent_tool_use_id", b.Actor))
	}
	if b.Purpose != "" {
		attrs = append(attrs, attribute.String(attrObsMetaPrefix+"purpose", b.Purpose))
	}
	if b.Instalment {
		attrs = append(attrs, attribute.Bool(attrObsMetaPrefix+"instalment", true))
	}
	if b.SessionFile != "" {
		attrs = append(attrs, attribute.String(attrObsMetaPrefix+"session_file", b.SessionFile))
	}
	if b.SessionLine > 0 {
		attrs = append(attrs, attribute.Int(attrObsMetaPrefix+"session_line", b.SessionLine))
	}
	if n := detailInt(b.Detail, "pre_messages"); n > 0 {
		attrs = append(attrs, attribute.Int64(attrObsMetaPrefix+"pre_messages", n))
	}

	// A helper call's turn is the ledger's own bookkeeping, never a traced
	// turn with a root span, so it gets a trace of its own.
	turnID := b.TurnID
	if !tracedTurn(b) {
		turnID = ""
	}
	ctx := context.Background()
	var traceID trace.TraceID
	if turnID != "" {
		traceID = TraceIDForTurn(turnID)
		parent := RootSpanID(turnID)
		if b.Actor != "" {
			parent = SubagentSpanID(turnID, b.Actor, 1)
		}
		ctx = parentContext(ctx, traceID, parent)
	} else {
		traceID = RowTraceID(b.Session, callType, start)
		backend := "delegated"
		if b.Backend == accounting.BackendAPI {
			backend = "api"
		}
		attrs = append(attrs,
			attribute.String(attrTraceName, callType),
			attribute.StringSlice(attrTraceTags, []string{"agent:" + agent, "call_type:" + callType, "backend:" + backend}),
		)
	}
	// The generation span itself is also keyed by the actor (NOT the agent,
	// which is the same for every call of a turn — using it here would
	// collapse every subagent's span into one).
	spanID := GenerationSpanID(turnID, b.Session, callType, b.Actor, b.Model,
		strconv.FormatInt(start.UnixNano(), 10), strconv.Itoa(out), strconv.FormatBool(b.Instalment))
	ctx = withIDs(ctx, traceID, spanID)
	_, span := tr.Start(ctx, callType, trace.WithTimestamp(start), trace.WithAttributes(attrs...))
	span.End(trace.WithTimestamp(end))
}

// tracedTurn reports whether b's turn is a conversation turn the tracer opened
// a root span for — every call booked on one, a summary, spawn or compaction
// that turn made included. A helper call no turn made is booked on a turn of
// its own (source compaction or system), which has no root span.
func tracedTurn(b accounting.Booking) bool {
	return b.TurnID != "" && b.TurnSource != accounting.SourceCompaction && b.TurnSource != accounting.SourceSystem
}

// callTypeOf names b the way its trace and metadata always have: a legacy
// row's pre-ledger call type, "conversation" for a direct-API turn's call,
// else the call's kind.
func callTypeOf(b accounting.Booking) string {
	if b.Kind == accounting.KindLegacy {
		if ct, _ := b.Detail["v1_call_type"].(string); ct != "" {
			return ct
		}
	}
	if b.Kind == accounting.KindCall && b.Backend == accounting.BackendAPI {
		return "conversation"
	}
	return b.Kind
}

// detailInt reads an integer from a call's detail, which holds Go values when
// booked and JSON numbers when read back.
func detailInt(d map[string]any, key string) int64 {
	switch v := d[key].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return 0
}

// recordCorrection is accounting.CorrectionHook: a #1918 correction moved
// spend from a parent call to a subagent call in api.db. An append-only sink cannot move
// money — re-sending either observation with new numbers would double-count
// (the ETL found this the hard way) — so the correction is recorded as a
// zero-cost "event" under the subagent span. Daily totals are unaffected
// (the move sums to zero); per-observation attribution differs from api.db
// by exactly the amount shown here.
func recordCorrection(c modelinfo.CostCorrection, parentTurn string) {
	tr, o, _, ok := current()
	if !ok || c.SubagentTurnID == "" || c.AgentID == "" {
		return
	}
	at := c.BilledAt
	if at.IsZero() {
		at = time.Now()
	}
	sessionKey, _, _ := strings.Cut(c.SubagentTurnID, "@")
	traceID := TraceIDForTurn(c.SubagentTurnID)
	ctx := parentContext(context.Background(), traceID, SubagentSpanID(c.SubagentTurnID, c.AgentID, 1))
	ctx = withIDs(ctx, traceID, EventSpanID("correction", c.SubagentTurnID, c.AgentID, c.Model,
		strconv.FormatInt(at.UnixNano(), 10)))
	_, span := tr.Start(ctx, "cost_correction", trace.WithTimestamp(at), trace.WithAttributes(
		attribute.String(attrObsType, "event"),
		attribute.String(attrObsLevel, "WARNING"),
		attribute.String(attrObsStatus, "spend re-attributed from parent turn "+parentTurn+" (api.db updated in place; this sink is append-only, so totals here keep the original split)"),
		attribute.String(attrUserID, session.AgentIDFromAnyKey(sessionKey)),
		attribute.String(attrSessionID, sessionKey),
		attribute.String(attrEnvironment, o.Environment),
		attribute.String(attrObsMetaPrefix+"source", "foci"),
		attribute.String(attrObsMetaPrefix+"parent_turn_id", parentTurn),
		attribute.String(attrObsMetaPrefix+"subagent_turn_id", c.SubagentTurnID),
		attribute.String(attrObsMetaPrefix+"subagent_tool_use_id", c.AgentID),
		attribute.String(attrObsMetaPrefix+"model_raw", c.Model),
		attribute.Float64(attrObsMetaPrefix+"moved_usd", c.CostUSD),
		attribute.Float64(attrObsMetaPrefix+"ttl_surcharge_usd", c.TTLSurchargeUSD),
		attribute.Int(attrObsMetaPrefix+"moved_input", c.Counts.Input),
		attribute.Int(attrObsMetaPrefix+"moved_output", c.Counts.Output),
		attribute.Int(attrObsMetaPrefix+"moved_cache_read", c.Counts.CacheRead),
		attribute.Int(attrObsMetaPrefix+"moved_cache_write", c.Counts.CacheWrite),
	))
	span.End(trace.WithTimestamp(at))
}
