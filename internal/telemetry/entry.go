package telemetry

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"foci/internal/log"
	"foci/internal/modelinfo"
	"foci/internal/session"
)

// recordEntry is log.APIHook: one api.db row → one "generation" observation.
//
// Exactly one, and the only place cost is billed, is the invariant that makes
// Langfuse's daily totals equal api.db's. The root/tool/subagent spans carry
// cost only as read-only metadata. Parentage is derived from the row alone:
// a subagent_turn row hangs under its subagent's span (both are keyed by the
// Agent tool_use id), any other row with a turn id under that turn's root,
// and a row with no turn id (compaction, summariser, spawn) becomes its own
// one-observation trace named after its call type.
//
// instalment is true when AccumulateSubagentRow FOLDED this spend into an
// existing db row rather than inserting: the db holds one row per delegation,
// but an append-only sink cannot update, so each instalment is its own
// observation and the delegation's cost is their sum — same arithmetic as
// the JSONL mirror, which also receives every instalment.
func recordEntry(e log.APIEntry, instalment bool) {
	tr, o, _, ok := current()
	if !ok {
		return
	}
	start := e.Timestamp
	if start.IsZero() {
		start = time.Now()
	}
	end := start.Add(time.Duration(e.DurationMS) * time.Millisecond)
	if end.Before(start) {
		end = start
	}
	// e.AgentID is populated on every row by the writer now (#1946) — no need
	// to re-derive it from the session key here.
	agent := e.AgentID

	// Token scope: the turn_* group is what calculated_cost_usd priced; the
	// un-suffixed four are the final cycle's context fill (#1854) and only
	// coincide with the turn on a single-call row.
	counts := e.PricedCounts()
	scope := "snapshot"
	if e.Turn != nil {
		scope = "turn"
	}
	usage := map[string]int{
		"input":                       counts.Input,
		"output":                      counts.Output,
		"cache_read_input_tokens":     counts.CacheRead,
		"cache_creation_input_tokens": counts.CacheWrite,
		"total":                       counts.Input + counts.Output + counts.CacheRead + counts.CacheWrite,
	}
	usageB, _ := json.Marshal(usage)
	cost := 0.0
	if e.CalculatedCostUSD != nil {
		cost = *e.CalculatedCostUSD
	}
	costB, _ := json.Marshal(map[string]float64{"total": cost})

	callType := e.CallType
	if callType == "" {
		callType = "conversation"
	}
	attrs := []attribute.KeyValue{
		attribute.String(attrObsType, "generation"),
		attribute.String(attrObsUsage, string(usageB)),
		attribute.String(attrObsCost, string(costB)),
		attribute.String(attrObsLevel, "DEFAULT"),
		attribute.String(attrUserID, agent),
		attribute.String(attrSessionID, e.Session),
		attribute.String(attrEnvironment, o.Environment),
		attribute.String(attrObsMetaPrefix+"source", "foci"),
		attribute.String(attrObsMetaPrefix+"call_type", callType),
		attribute.String(attrObsMetaPrefix+"token_scope", scope),
		attribute.String(attrObsMetaPrefix+"model_raw", e.Model),
		attribute.String(attrObsMetaPrefix+"provider", e.Provider),
		attribute.String(attrObsMetaPrefix+"stop_reason", e.StopReason),
		attribute.Int64(attrObsMetaPrefix+"duration_ms", e.DurationMS),
		attribute.Int(attrObsMetaPrefix+"context_tokens", e.Input+e.CacheRead+e.CacheWrite),
	}
	if m := modelinfo.Normalize(e.Model); m != "" && e.Model != "<synthetic>" {
		attrs = append(attrs, attribute.String(attrObsModel, m))
	}
	if e.ProvidedCostUSD != nil {
		attrs = append(attrs, attribute.Float64(attrObsMetaPrefix+"backend_cost_usd", *e.ProvidedCostUSD))
	}
	if e.TurnID != "" {
		attrs = append(attrs, attribute.String(attrObsMetaPrefix+"turn_id", e.TurnID))
	}
	if e.SubagentID != "" {
		attrs = append(attrs, attribute.String(attrObsMetaPrefix+"subagent_tool_use_id", e.SubagentID))
	}
	if e.Purpose != "" {
		attrs = append(attrs, attribute.String(attrObsMetaPrefix+"purpose", e.Purpose))
	}
	if instalment {
		attrs = append(attrs, attribute.Bool(attrObsMetaPrefix+"instalment", true))
	}
	if e.SessionFile != "" {
		attrs = append(attrs, attribute.String(attrObsMetaPrefix+"session_file", e.SessionFile))
	}
	if e.SessionLine > 0 {
		attrs = append(attrs, attribute.Int(attrObsMetaPrefix+"session_line", e.SessionLine))
	}
	if e.PreMessages > 0 {
		attrs = append(attrs, attribute.Int(attrObsMetaPrefix+"pre_messages", e.PreMessages))
	}

	ctx := context.Background()
	var traceID trace.TraceID
	if e.TurnID != "" {
		traceID = TraceIDForTurn(e.TurnID)
		parent := RootSpanID(e.TurnID)
		if e.IsSubagent() && e.SubagentID != "" {
			parent = SubagentSpanID(e.TurnID, e.SubagentID, 1)
		}
		ctx = parentContext(ctx, traceID, parent)
	} else {
		traceID = RowTraceID(e.Session, callType, start)
		attrs = append(attrs,
			attribute.String(attrTraceName, callType),
			attribute.StringSlice(attrTraceTags, []string{"agent:" + agent, "call_type:" + callType, "backend:" + backendFromEntry(e)}),
		)
	}
	// The generation span itself is also keyed by SubagentID (NOT AgentID,
	// which #1946 made the same for every row of a turn — using it here would
	// collapse every subagent's span into one).
	spanID := GenerationSpanID(e.TurnID, e.Session, callType, e.SubagentID, e.Model,
		strconv.FormatInt(start.UnixNano(), 10), strconv.Itoa(counts.Output), strconv.FormatBool(instalment))
	ctx = withIDs(ctx, traceID, spanID)
	_, span := tr.Start(ctx, callType, trace.WithTimestamp(start), trace.WithAttributes(attrs...))
	span.End(trace.WithTimestamp(end))
}

// recordCorrection is log.CorrectionHook: a #1918 correction moved spend from
// a parent row to a subagent row in api.db. An append-only sink cannot move
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
		attribute.Float64(attrObsMetaPrefix+"stranded_usd", c.StrandedUSD),
		attribute.Int(attrObsMetaPrefix+"moved_input", c.Counts.Input),
		attribute.Int(attrObsMetaPrefix+"moved_output", c.Counts.Output),
		attribute.Int(attrObsMetaPrefix+"moved_cache_read", c.Counts.CacheRead),
		attribute.Int(attrObsMetaPrefix+"moved_cache_write", c.Counts.CacheWrite),
	))
	span.End(trace.WithTimestamp(at))
}

// backendFromEntry guesses the transport for a turn-less row from what the
// row says about itself; turn rows get the real backend from the root span.
func backendFromEntry(e log.APIEntry) string {
	switch e.CallType {
	case "delegated_turn", "subagent_turn":
		return "delegated"
	}
	return "api"
}
