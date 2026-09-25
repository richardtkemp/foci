package telemetry

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"foci/internal/modelinfo"
	"foci/internal/provider"
	"foci/internal/turnevent"
)

// Langfuse's OTel endpoint reads these attribute names (the same ones its
// Python SDK emits). Trace-level ones are set on the root span; observation
// ones on every span.
const (
	attrObsType       = "langfuse.observation.type" // span|generation|event|agent|tool
	attrObsInput      = "langfuse.observation.input"
	attrObsOutput     = "langfuse.observation.output"
	attrObsLevel      = "langfuse.observation.level" // DEBUG|DEFAULT|WARNING|ERROR
	attrObsStatus     = "langfuse.observation.status_message"
	attrObsModel      = "langfuse.observation.model.name"
	attrObsUsage      = "langfuse.observation.usage_details" // JSON
	attrObsCost       = "langfuse.observation.cost_details"  // JSON
	attrObsMetaPrefix = "langfuse.observation.metadata."
	attrTraceName     = "langfuse.trace.name"
	attrTraceInput    = "langfuse.trace.input"
	attrTraceOutput   = "langfuse.trace.output"
	attrTraceTags     = "langfuse.trace.tags"
	attrEnvironment   = "langfuse.environment"
	attrUserID        = "user.id"    // → Langfuse user: the foci agent
	attrSessionID     = "session.id" // → Langfuse session: the foci session key
)

// TurnInfo is what the agent knows about a turn at the moment its identity
// (TurnID) is fixed — before the model is even resolved; the prompt and
// requested model follow in SetInput, the answering model in Complete.
type TurnInfo struct {
	TurnID     string // api.db turn_id: "<session>@<StartedAt UnixNano>"
	SessionKey string
	AgentID    string
	Trigger    string // raw trigger label (telegram, keepalive, session_notify, …)
	Via        string // the [meta] via= classification of Trigger
	Backend    string // api | claude-code | opencode | codex | …
	StartedAt  time.Time
	ReceivedAt time.Time // platform receipt time; zero for system turns
	ChatID     int64
	UserID     string // platform user id, as the platform reports it
	Username   string
	// Purpose labels a batch run (consolidation, nudge_extraction, summary —
	// delegator.BatchPurpose*); empty for every other turn (#1962). Tagged so
	// a rubric can select, say, every consolidation trace.
	Purpose string
}

// Turn is the live tracing state of one turn: the root span plus the child
// spans still open. Built by NewTurnSink; driven by the sink's events and by
// the orchestrator's explicit calls (Begin, SetInput). All methods are safe
// to call on a nil *Turn (tracing off) and after the turn has ended (late
// events are dropped, not crashed).
type Turn struct {
	mu    sync.Mutex
	info  TurnInfo
	ctx   context.Context // root span context, for starting children
	root  trace.Span
	tools map[string]toolSpan // open tool spans by tool_use id

	texts []string // intermediate text blocks, in order
	// The API path streams thinking as deltas AND then emits the whole
	// block; the delegated path streams deltas only. Blocks win when present
	// so the same reasoning is not recorded twice.
	thinkingDeltas strings.Builder
	thinkingBlocks strings.Builder
	toolCount      int
	subagents      int
	retries        int
	link           *Link // caller's turn, for an injected cross-agent turn
	begun          bool
	ended          bool
}

type toolSpan struct {
	span trace.Span
	name string
}

// thinkingCap bounds the thinking text retained per turn; extended thinking
// can run to megabytes and the field is metadata, not the record.
const thinkingCap = 256 << 10

// Begin opens the root span. Called by the orchestrator the moment TurnID is
// known (StartedAt set), which is before ComposePrompt — so the input is
// attached later by SetInput. Idempotent.
func (t *Turn) Begin(info TurnInfo) {
	if t == nil {
		return
	}
	tr, o, _, ok := current()
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.begun || info.TurnID == "" {
		return
	}
	t.begun = true
	t.info = info
	t.link = takePendingLink(info.SessionKey, info.Trigger)
	setActiveTurn(info.SessionKey, info.TurnID)

	tags := []string{
		"agent:" + info.AgentID,
		"via:" + info.Via,
		"trigger:" + info.Trigger,
		"backend:" + info.Backend,
	}
	if t.link != nil {
		tags = append(tags, "from:"+t.link.FromAgent)
	}
	if info.Purpose != "" {
		tags = append(tags, "purpose:"+info.Purpose)
	}
	attrs := []attribute.KeyValue{
		attribute.String(attrObsType, "agent"),
		attribute.String(attrTraceName, "turn"),
		attribute.StringSlice(attrTraceTags, tags),
		attribute.String(attrUserID, info.AgentID),
		attribute.String(attrSessionID, info.SessionKey),
		attribute.String(attrEnvironment, o.Environment),
		attribute.String(attrObsMetaPrefix+"turn_id", info.TurnID),
		attribute.String(attrObsMetaPrefix+"trigger", info.Trigger),
		attribute.String(attrObsMetaPrefix+"via", info.Via),
		attribute.String(attrObsMetaPrefix+"backend", info.Backend),
		attribute.String(attrObsMetaPrefix+"source", "foci"),
	}
	if info.ChatID != 0 {
		attrs = append(attrs, attribute.Int64(attrObsMetaPrefix+"chat_id", info.ChatID))
	}
	if info.Purpose != "" {
		attrs = append(attrs, attribute.String(attrObsMetaPrefix+"purpose", info.Purpose))
	}
	if info.UserID != "" {
		attrs = append(attrs, attribute.String(attrObsMetaPrefix+"platform_user_id", info.UserID))
	}
	if info.Username != "" {
		attrs = append(attrs, attribute.String(attrObsMetaPrefix+"username", info.Username))
	}
	if !info.ReceivedAt.IsZero() && !info.ReceivedAt.Equal(info.StartedAt) {
		attrs = append(attrs,
			attribute.String(attrObsMetaPrefix+"received_at", info.ReceivedAt.Format(time.RFC3339Nano)),
			attribute.Int64(attrObsMetaPrefix+"queue_wait_ms", info.StartedAt.Sub(info.ReceivedAt).Milliseconds()),
		)
	}
	if l := t.link; l != nil {
		attrs = append(attrs,
			attribute.String(attrObsMetaPrefix+"parent_trace_id", l.TraceID.String()),
			attribute.String(attrObsMetaPrefix+"parent_turn_id", l.TurnID),
			attribute.String(attrObsMetaPrefix+"caller_session", l.FromSession),
			attribute.String(attrObsMetaPrefix+"caller_agent", l.FromAgent),
		)
	}

	ctx := withIDs(context.Background(), TraceIDForTurn(info.TurnID), RootSpanID(info.TurnID))
	t.ctx, t.root = tr.Start(ctx, "turn",
		trace.WithTimestamp(info.StartedAt),
		trace.WithAttributes(attrs...),
	)
	t.tools = make(map[string]toolSpan)
}

// SetInput attaches the prompt exactly as sent to the backend (meta header,
// nudges and all) — the record of what the model saw, not what the human typed.
func (t *Turn) SetInput(prompt, model string) {
	if t == nil {
		return
	}
	_, o, r, ok := current()
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.begun || t.ended {
		return
	}
	t.root.SetAttributes(attribute.Int(attrObsMetaPrefix+"input_chars", len(prompt)))
	if model != "" {
		t.root.SetAttributes(attribute.String(attrObsMetaPrefix+"model_requested", model))
	}
	if !o.Content {
		return
	}
	in, n := field(o, r, prompt)
	t.root.SetAttributes(
		attribute.String(attrObsInput, in),
		attribute.String(attrTraceInput, in),
	)
	if n > 0 {
		t.root.SetAttributes(attribute.Int(attrObsMetaPrefix+"input_redactions", n))
	}
}

// ToolStart opens a "tool" child. args is the tool_use input JSON (or, for
// codex, the display command).
func (t *Turn) ToolStart(id, name string, args []byte) {
	if t == nil {
		return
	}
	tr, o, r, ok := current()
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.begun || t.ended {
		return
	}
	if _, dup := t.tools[id]; dup {
		return // --include-partial-messages replays the tool_use block
	}
	t.toolCount++
	attrs := []attribute.KeyValue{
		attribute.String(attrObsType, "tool"),
		attribute.String(attrObsMetaPrefix+"tool_use_id", id),
		attribute.Int(attrObsMetaPrefix+"input_chars", len(args)),
		attribute.String(attrUserID, t.info.AgentID),
		attribute.String(attrSessionID, t.info.SessionKey),
		attribute.String(attrEnvironment, o.Environment),
	}
	// send_to_session is the cross-agent edge: name the target so the two
	// traces can be joined from either end (the target's turn records
	// parent_trace_id; this span records where it went).
	if name == "send_to_session" || name == "foci_send_to_session" {
		if target := jsonField(args, "session_key"); target != "" {
			attrs = append(attrs, attribute.String(attrObsMetaPrefix+"target_session", target))
		}
	}
	if o.Content {
		in, n := field(o, r, string(args))
		attrs = append(attrs, attribute.String(attrObsInput, in))
		if n > 0 {
			attrs = append(attrs, attribute.Int(attrObsMetaPrefix+"input_redactions", n))
		}
	}
	ctx := withIDs(t.ctx, trace.TraceID{}, ToolSpanID(t.info.TurnID, id))
	_, span := tr.Start(ctx, name, trace.WithAttributes(attrs...))
	t.tools[id] = toolSpan{span: span, name: name}
}

// ToolEnd closes the tool child with its output. A result whose call was
// never seen (a tool started in an earlier turn — run_in_background Bash —
// or a replay) gets a zero-length span so the output is still recorded.
func (t *Turn) ToolEnd(id, name, output string, isError bool) {
	if t == nil {
		return
	}
	tr, o, r, ok := current()
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.begun {
		return
	}
	ts, found := t.tools[id]
	if !found {
		if t.ended {
			return
		}
		ctx := withIDs(t.ctx, trace.TraceID{}, ToolSpanID(t.info.TurnID, id))
		_, span := tr.Start(ctx, name, trace.WithAttributes(
			attribute.String(attrObsType, "tool"),
			attribute.String(attrObsMetaPrefix+"tool_use_id", id),
			attribute.Bool(attrObsMetaPrefix+"start_unobserved", true),
			attribute.String(attrUserID, t.info.AgentID),
			attribute.String(attrSessionID, t.info.SessionKey),
			attribute.String(attrEnvironment, o.Environment),
		))
		ts = toolSpan{span: span, name: name}
	}
	delete(t.tools, id)
	ts.span.SetAttributes(attribute.Int(attrObsMetaPrefix+"output_chars", len(output)))
	if o.Content {
		out, n := field(o, r, output)
		ts.span.SetAttributes(attribute.String(attrObsOutput, out))
		if n > 0 {
			ts.span.SetAttributes(attribute.Int(attrObsMetaPrefix+"output_redactions", n))
		}
	}
	if isError {
		ts.span.SetAttributes(attribute.String(attrObsLevel, "ERROR"))
		ts.span.SetStatus(codes.Error, "tool error")
	}
	ts.span.End()
}

// Text records an intermediate text block (a reply delivered mid-turn).
func (t *Turn) Text(text string) {
	if t == nil || text == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.begun || t.ended {
		return
	}
	t.texts = append(t.texts, text)
}

// ThinkingDelta accumulates a streamed extended-thinking fragment.
func (t *Turn) ThinkingDelta(s string) {
	if t == nil || s == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.begun || t.ended || t.thinkingDeltas.Len() >= thinkingCap {
		return
	}
	t.thinkingDeltas.WriteString(s)
}

// ThinkingBlock records a complete extended-thinking block.
func (t *Turn) ThinkingBlock(s string) {
	if t == nil || s == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.begun || t.ended || t.thinkingBlocks.Len() >= thinkingCap {
		return
	}
	if t.thinkingBlocks.Len() > 0 {
		t.thinkingBlocks.WriteString("\n\n")
	}
	t.thinkingBlocks.WriteString(s)
}

// thinking returns the reasoning text to export: whole blocks when the
// transport delivered them, else the streamed deltas.
func (t *Turn) thinking() string {
	if t.thinkingBlocks.Len() > 0 {
		return t.thinkingBlocks.String()
	}
	return t.thinkingDeltas.String()
}

// Retry records an upstream retry as a span event on the root.
func (t *Turn) Retry(attempt int, endpoint string, err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.begun || t.ended {
		return
	}
	t.retries++
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	t.root.AddEvent("retry", trace.WithAttributes(
		attribute.Int("attempt", attempt),
		attribute.String("endpoint", endpoint),
		attribute.String("error", msg),
	))
}

// Complete closes the root span: output, model, usage (as metadata — cost and
// usage are BILLED on the generation observations the api.db hook writes, so
// the root carries them for reading only, never for summing), the system
// prompt record, and error status. Any tool span still open is closed as
// unresolved so the export is never held hostage to a lost hook.
func (t *Turn) Complete(finalText, model string, usage *provider.Usage, cost float64, err error) {
	if t == nil {
		return
	}
	tr, o, r, ok := current()
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.begun || t.ended {
		return
	}
	t.ended = true
	clearActiveTurn(t.info.SessionKey, t.info.TurnID)

	for id, ts := range t.tools {
		ts.span.SetAttributes(attribute.Bool(attrObsMetaPrefix+"end_unobserved", true))
		ts.span.End()
		delete(t.tools, id)
	}

	attrs := []attribute.KeyValue{
		attribute.Int(attrObsMetaPrefix+"tool_calls", t.toolCount),
		attribute.Int(attrObsMetaPrefix+"subagent_runs", t.subagents),
		attribute.Int(attrObsMetaPrefix+"intermediate_texts", len(t.texts)),
		attribute.Int(attrObsMetaPrefix+"output_chars", len(finalText)),
		attribute.Int(attrObsMetaPrefix+"thinking_chars", len(t.thinking())),
	}
	if t.retries > 0 {
		attrs = append(attrs, attribute.Int(attrObsMetaPrefix+"retries", t.retries))
	}
	if model != "" {
		attrs = append(attrs,
			attribute.String(attrObsMetaPrefix+"model", modelinfo.Normalize(model)),
			attribute.String(attrObsMetaPrefix+"model_raw", model),
		)
	}
	if usage != nil {
		attrs = append(attrs, attribute.String(attrObsMetaPrefix+"usage", usageJSON(usage)))
		if len(usage.Subagents) > 0 {
			attrs = append(attrs, attribute.Int(attrObsMetaPrefix+"subagent_rows", len(usage.Subagents)))
		}
	}
	// The turn total INCLUDING subagent shares — what the sink header shows.
	attrs = append(attrs, attribute.Float64(attrObsMetaPrefix+"cost_usd", cost))

	output := finalText
	if output == "" && len(t.texts) > 0 {
		output = strings.Join(t.texts, "\n\n")
	}
	if o.Content {
		out, n := field(o, r, output)
		attrs = append(attrs,
			attribute.String(attrObsOutput, out),
			attribute.String(attrTraceOutput, out),
		)
		if n > 0 {
			attrs = append(attrs, attribute.Int(attrObsMetaPrefix+"output_redactions", n))
		}
		if len(t.texts) > 1 || (len(t.texts) == 1 && t.texts[0] != finalText) {
			if b, jerr := json.Marshal(t.texts); jerr == nil {
				s, _ := field(o, r, string(b))
				attrs = append(attrs, attribute.String(attrObsMetaPrefix+"texts", s))
			}
		}
		if th := t.thinking(); th != "" {
			s, _ := field(o, r, th)
			attrs = append(attrs, attribute.String(attrObsMetaPrefix+"thinking", s))
		}
	}
	if err != nil {
		attrs = append(attrs,
			attribute.String(attrObsLevel, "ERROR"),
			attribute.String(attrObsStatus, err.Error()),
		)
		t.root.SetStatus(codes.Error, err.Error())
	}
	t.root.SetAttributes(attrs...)

	// System prompt: hash + length on every turn; the text once per session
	// per distinct hash, as a zero-length child event at the turn's start.
	if sp := systemPromptFor(t.info.SessionKey); sp != nil {
		t.root.SetAttributes(
			attribute.String(attrObsMetaPrefix+"system_prompt_sha256", sp.hash),
			attribute.Int(attrObsMetaPrefix+"system_prompt_chars", len(sp.text)),
		)
		if o.Content && o.SystemPrompt && markSystemPromptExported(t.info.SessionKey, sp.hash) {
			text, n := field(o, r, sp.text)
			ctx := withIDs(t.ctx, trace.TraceID{}, EventSpanID("system_prompt", t.info.TurnID, sp.hash))
			_, ev := tr.Start(ctx, "system_prompt",
				trace.WithTimestamp(t.info.StartedAt),
				trace.WithAttributes(
					attribute.String(attrObsType, "event"),
					attribute.String(attrObsInput, text),
					attribute.String(attrObsMetaPrefix+"sha256", sp.hash),
					attribute.Int(attrObsMetaPrefix+"chars", len(sp.text)),
					attribute.Int(attrObsMetaPrefix+"redactions", n),
					attribute.String(attrObsMetaPrefix+"source", sp.source),
					attribute.String(attrUserID, t.info.AgentID),
					attribute.String(attrSessionID, t.info.SessionKey),
					attribute.String(attrEnvironment, o.Environment),
				))
			ev.End(trace.WithTimestamp(t.info.StartedAt))
		}
	}
	t.root.End()
}

// TurnID returns the turn's identity, or "" before Begin.
func (t *Turn) TurnID() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.info.TurnID
}

// usageJSON renders a provider.Usage in Langfuse's usage_details vocabulary.
func usageJSON(u *provider.Usage) string {
	m := map[string]any{
		"input":                       u.InputTokens,
		"output":                      u.OutputTokens,
		"cache_read_input_tokens":     u.CacheReadInputTokens,
		"cache_creation_input_tokens": u.CacheCreationInputTokens,
	}
	if u.Turn != nil {
		m["turn"] = map[string]int{
			"input":                       u.Turn.Input,
			"output":                      u.Turn.Output,
			"cache_read_input_tokens":     u.Turn.CacheRead,
			"cache_creation_input_tokens": u.Turn.CacheWrite,
		}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// jsonField extracts one top-level string field from a JSON object without
// failing on the rest of it.
func jsonField(raw []byte, key string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(m[key], &s) != nil {
		return ""
	}
	return s
}

// ---------------------------------------------------------------------------
// Sink wrapper
// ---------------------------------------------------------------------------

type turnSink struct {
	inner turnevent.Sink
	t     *Turn
}

type turnKey struct{}

// NewTurnSink wraps inner so the turn's events also drive a trace. Returns
// inner unchanged and a nil *Turn when tracing is off, so callers can wrap
// unconditionally.
func NewTurnSink(inner turnevent.Sink) (turnevent.Sink, *Turn) {
	if !Enabled() || inner == nil {
		return inner, nil
	}
	t := &Turn{}
	return &turnSink{inner: inner, t: t}, t
}

// WithTurn stores the turn on ctx so the orchestrator can reach it from
// inside the turn pipeline (Begin, SetInput).
func WithTurn(ctx context.Context, t *Turn) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, turnKey{}, t)
}

// TurnFromContext returns the turn on ctx, or nil (every Turn method is
// nil-safe, so callers need not check).
func TurnFromContext(ctx context.Context) *Turn {
	t, _ := ctx.Value(turnKey{}).(*Turn)
	return t
}

func (s *turnSink) DeliversToPlatform() bool { return s.inner.DeliversToPlatform() }

// Unwrap exposes the decorated sink so the agent's session router can tell a
// wrapped router from a fresh per-turn sink. Without it, HandleMessage's
// wrap of a platform turn (whose ctx sink IS the router) defeated the
// orchestrator's identity guard and the wrapper was registered into the
// router it forwards to — a mutual recursion on the first event (#1944).
func (s *turnSink) Unwrap() turnevent.Sink { return s.inner }

var _ turnevent.Unwrapper = (*turnSink)(nil)

func (s *turnSink) Emit(ctx context.Context, ev turnevent.Event) {
	switch e := ev.(type) {
	case turnevent.ToolCall:
		s.t.ToolStart(e.ID, e.Name, e.Args)
	case turnevent.ToolResult:
		s.t.ToolEnd(e.ID, e.Name, e.Output, e.IsError)
	case turnevent.SubagentStart:
		s.t.SubagentStart(e.GroupKey, e.Label, e.Prompt, e.RunIndex)
	case turnevent.SubagentText:
		s.t.SubagentText(e.GroupKey, e.Text, e.RunIndex)
	case turnevent.SubagentPrompt:
		s.t.SubagentPrompt(e.GroupKey, e.Prompt, e.RunIndex)
	case turnevent.SubagentEnd:
		s.t.SubagentEnd(e.GroupKey, e.RunIndex)
	case turnevent.TextBlock:
		if e.Phase == turnevent.PhaseIntermediate {
			s.t.Text(e.Text)
		}
	case turnevent.ThinkingDelta:
		s.t.ThinkingDelta(e.Delta)
	case turnevent.ThinkingBlock:
		s.t.ThinkingBlock(e.Text)
	case turnevent.RetryNotice:
		s.t.Retry(e.Attempt, e.Endpoint, e.Err)
	case turnevent.TurnComplete:
		s.t.Complete(e.FinalText, e.Model, e.Usage, e.Cost, e.Err)
	}
	s.inner.Emit(ctx, ev)
}
