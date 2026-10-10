package telemetry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"foci/internal/turnevent"
)

// This file pins #2298: the four free-text exports that used to bypass
// field — the retry event's error attribute, an errored turn's
// status_message and OTel status description, and a subagent run's label
// (span name + metadata) — must be redacted like every other exported
// text, and still exported with content = false. Every secret here is a
// made-up value (>= 8 chars, lower-case letters and digits only), so only
// value-based redaction can scrub it; assertion messages name the field,
// never the value.

// eventAttr looks up one attribute on the i-th span event with the given
// name (0-based), so a test can read successive "retry" events. Fails when
// the event or the attribute is absent.
func eventAttr(t *testing.T, span *tracetest.SpanStub, i int, eventName, key string) attribute.Value {
	t.Helper()
	seen := 0
	for _, ev := range span.Events {
		if ev.Name != eventName {
			continue
		}
		if seen == i {
			for _, a := range ev.Attributes {
				if string(a.Key) == key {
					return a.Value
				}
			}
			t.Fatalf("event %q #%d has no attribute %q", eventName, i, key)
		}
		seen++
	}
	t.Fatalf("span has no event %q #%d", eventName, i)
	return attribute.Value{}
}

// spanByID returns the span with the given span id, or nil — the way to
// find a subagent span whose name is not predictable (it embeds the
// redacted label).
func spanByID(spans tracetest.SpanStubs, id trace.SpanID) *tracetest.SpanStub {
	for i := range spans {
		if spans[i].SpanContext.SpanID() == id {
			return &spans[i]
		}
	}
	return nil
}

// TestRetryErrorAttributeRedacted proves the retry event's error attribute
// on the root span goes through field: an upstream error that echoes a
// secret value exports [REDACTED], never the value, while attempt and
// endpoint keep their values. The folded nil-error arm pins that a retry
// without an error still exports an empty error attribute.
func TestRetryErrorAttributeRedacted(t *testing.T) {
	const secret = "errsecret0001"
	exp := setupTest(t, Options{
		Content:       true,
		MaxFieldBytes: 1 << 20,
		SecretValues:  func() []string { return []string{secret} },
	})
	sink, turn := NewTurnSink(turnevent.NopSink{})
	turn.Begin(TurnInfo{
		TurnID:     "agent/retry@1700000000000000000",
		SessionKey: "agent/retry",
		AgentID:    "agent",
		Trigger:    "test",
		Via:        "test",
		Backend:    "api",
		StartedAt:  time.Unix(0, 1700000000000000000),
	})
	sink.Emit(context.Background(), turnevent.RetryNotice{
		Attempt:  2,
		Endpoint: "Anthropic API",
		Err:      errors.New("upstream 500: x-api-key header echoed value " + secret),
	})
	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "done", Model: "claude-opus-5"})
	flush(t)

	root := findSpan(exp.GetSpans(), "turn")
	if root == nil {
		t.Fatal("no root span")
	}
	msg := eventAttr(t, root, 0, "retry", "error").AsString()
	if !strings.Contains(msg, "[REDACTED]") {
		t.Error(`retry event "error" attribute shows no [REDACTED] — the secret value echoed by the upstream error was not scrubbed`)
	}
	if strings.Contains(msg, secret) {
		t.Error(`retry event "error" attribute leaks the secret value echoed by the upstream error`)
	}
	if got := eventAttr(t, root, 0, "retry", "attempt").AsInt64(); got != 2 {
		t.Errorf(`retry event "attempt" attribute = %d, want 2 (redaction must not touch it)`, got)
	}
	if got := eventAttr(t, root, 0, "retry", "endpoint").AsString(); got != "Anthropic API" {
		t.Errorf(`retry event "endpoint" attribute = %q, want "Anthropic API" (redaction must not touch it)`, got)
	}

	// Characterisation arm (green before #2298 too, folded after the red
	// assertions): a nil-error retry still exports an empty error attribute.
	sink2, turn2 := NewTurnSink(turnevent.NopSink{})
	turn2.Begin(TurnInfo{
		TurnID:     "agent/retrynil@1700000000000000000",
		SessionKey: "agent/retrynil",
		AgentID:    "agent",
		Trigger:    "test",
		Via:        "test",
		Backend:    "api",
		StartedAt:  time.Unix(0, 1700000000000000000),
	})
	sink2.Emit(context.Background(), turnevent.RetryNotice{Attempt: 1, Endpoint: "Anthropic API"})
	sink2.Emit(context.Background(), turnevent.TurnComplete{FinalText: "done", Model: "claude-opus-5"})
	flush(t)
	root2 := spanByID(exp.GetSpans(), RootSpanID("agent/retrynil@1700000000000000000"))
	if root2 == nil {
		t.Fatal("no root span for the nil-error retry turn")
	}
	if got := eventAttr(t, root2, 0, "retry", "error").AsString(); got != "" {
		t.Errorf(`nil-error retry event "error" attribute = %q, want ""`, got)
	}
}

// TestCompleteErrorFieldsRedacted proves an errored turn's error text goes
// through field exactly once: langfuse.observation.status_message and the
// OTel status description both hold [REDACTED] in place of the secret,
// never the value, and are equal. No redaction count is recorded for it.
func TestCompleteErrorFieldsRedacted(t *testing.T) {
	const secret = "completesecret01"
	exp := setupTest(t, Options{
		Content:       true,
		MaxFieldBytes: 1 << 20,
		SecretValues:  func() []string { return []string{secret} },
	})
	sink, turn := NewTurnSink(turnevent.NopSink{})
	turn.Begin(TurnInfo{
		TurnID:     "agent/comp@1700000000000000000",
		SessionKey: "agent/comp",
		AgentID:    "agent",
		Trigger:    "test",
		Via:        "test",
		Backend:    "api",
		StartedAt:  time.Unix(0, 1700000000000000000),
	})
	sink.Emit(context.Background(), turnevent.TurnComplete{
		FinalText: "done",
		Model:     "claude-opus-5",
		Err:       errors.New("provider 429: request failed with token " + secret + " in the query"),
	})
	flush(t)

	root := findSpan(exp.GetSpans(), "turn")
	if root == nil {
		t.Fatal("no root span")
	}
	status := attrStr(t, root.Attributes, attrObsStatus)
	if !strings.Contains(status, "[REDACTED]") {
		t.Error("langfuse.observation.status_message shows no [REDACTED] — the secret value echoed by the turn error was not scrubbed")
	}
	if strings.Contains(status, secret) {
		t.Error("langfuse.observation.status_message leaks the secret value echoed by the turn error")
	}
	desc := root.Status.Description
	if !strings.Contains(desc, "[REDACTED]") {
		t.Error("root span status description shows no [REDACTED] — the secret value echoed by the turn error was not scrubbed")
	}
	if strings.Contains(desc, secret) {
		t.Error("root span status description leaks the secret value echoed by the turn error")
	}
	if status != desc {
		t.Error("langfuse.observation.status_message and the span status description differ — both must carry the same redacted error text")
	}
	// No redaction count belongs on these fields (requirement: no new
	// attributes): nothing records how many replacements the error path made.
	for _, a := range root.Attributes {
		if strings.HasSuffix(string(a.Key), "redactions") {
			t.Errorf("attribute %q must not be recorded for the redacted error text — the error path records no redaction count", string(a.Key))
		}
	}
}

// TestSubagentLabelRedacted proves the Agent tool's description (written by
// the model) is redacted before it reaches the subagent span name or the
// label metadata attribute. The span is found by its span id, since the
// name embeds the (here secret, so unpredictable-when-raw) label. The
// folded empty-label arm pins that an empty label still names the span
// exactly "subagent".
func TestSubagentLabelRedacted(t *testing.T) {
	const secret = "labelsecret0001"
	exp := setupTest(t, Options{
		Content:       true,
		MaxFieldBytes: 1 << 20,
		SecretValues:  func() []string { return []string{secret} },
	})
	const turnID = "agent/sub@1700000000000000000"
	sink, turn := NewTurnSink(turnevent.NopSink{})
	turn.Begin(TurnInfo{
		TurnID:     turnID,
		SessionKey: "agent/sub",
		AgentID:    "agent",
		Trigger:    "test",
		Via:        "test",
		Backend:    "claude-code",
		StartedAt:  time.Unix(0, 1700000000000000000),
	})
	sink.Emit(context.Background(), turnevent.SubagentStart{
		GroupKey:  "toolu_l1",
		Label:     "check the vault for " + secret,
		RunIndex:  1,
		Prompt:    "go look",
	})
	sink.Emit(context.Background(), turnevent.SubagentEnd{GroupKey: "toolu_l1", RunIndex: 1})
	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "done", Model: "claude-opus-5"})
	flush(t)
	spans := exp.GetSpans()

	sub := spanByID(spans, SubagentSpanID(turnID, "toolu_l1", 1))
	if sub == nil {
		t.Fatal("no span with the expected subagent span id")
	}
	if strings.Contains(sub.Name, secret) {
		t.Error("subagent span name leaks the secret value quoted by the model-written label")
	}
	if !strings.Contains(sub.Name, "[REDACTED]") {
		t.Error("subagent span name shows no [REDACTED] — the model-written label was not scrubbed")
	}
	if !strings.HasPrefix(sub.Name, "subagent: ") {
		t.Errorf("subagent span name %q lost the %q prefix", sub.Name, "subagent: ")
	}
	lbl := attrStr(t, sub.Attributes, attrObsMetaPrefix+"label")
	if strings.Contains(lbl, secret) {
		t.Error("subagent label metadata attribute leaks the secret value quoted by the model-written label")
	}
	if !strings.Contains(lbl, "[REDACTED]") {
		t.Error("subagent label metadata attribute shows no [REDACTED] — the model-written label was not scrubbed")
	}

	// Characterisation arm (green before #2298 too, folded after the red
	// assertions): an empty label still names the span exactly "subagent".
	sink2, turn2 := NewTurnSink(turnevent.NopSink{})
	const turnID2 = "agent/subempty@1700000000000000000"
	turn2.Begin(TurnInfo{
		TurnID:     turnID2,
		SessionKey: "agent/subempty",
		AgentID:    "agent",
		Trigger:    "test",
		Via:        "test",
		Backend:    "claude-code",
		StartedAt:  time.Unix(0, 1700000000000000000),
	})
	sink2.Emit(context.Background(), turnevent.SubagentStart{GroupKey: "toolu_l2", Label: "", RunIndex: 1, Prompt: "go"})
	sink2.Emit(context.Background(), turnevent.SubagentEnd{GroupKey: "toolu_l2", RunIndex: 1})
	sink2.Emit(context.Background(), turnevent.TurnComplete{FinalText: "done", Model: "claude-opus-5"})
	flush(t)
	plain := spanByID(exp.GetSpans(), SubagentSpanID(turnID2, "toolu_l2", 1))
	if plain == nil {
		t.Fatal("no span with the expected empty-label subagent span id")
	}
	if plain.Name != "subagent" {
		t.Errorf("empty-label subagent span name = %q, want exactly %q", plain.Name, "subagent")
	}
}

// TestErrorAndLabelFieldsWithContentOff pins requirement "not gated on
// content": with content = false the retry error attribute, the errored
// turn's status_message and status description, and the subagent label are
// still exported — and still redacted. They are operational data, not
// conversation content.
func TestErrorAndLabelFieldsWithContentOff(t *testing.T) {
	const secret = "nocontentsrc01"
	newOpts := func() Options {
		return Options{
			Content:       false,
			MaxFieldBytes: 1 << 20,
			SecretValues:  func() []string { return []string{secret} },
		}
	}
	ctx := context.Background()

	t.Run("retry error attribute", func(t *testing.T) {
		exp := setupTest(t, newOpts())
		sink, turn := NewTurnSink(turnevent.NopSink{})
		turn.Begin(TurnInfo{
			TurnID:     "agent/ncretry@1700000000000000000",
			SessionKey: "agent/ncretry",
			AgentID:    "agent",
			Trigger:    "test",
			Via:        "test",
			Backend:    "api",
			StartedAt:  time.Unix(0, 1700000000000000000),
		})
		sink.Emit(ctx, turnevent.RetryNotice{
			Attempt:  1,
			Endpoint: "Anthropic API",
			Err:      errors.New("upstream 500: echoed key " + secret),
		})
		sink.Emit(ctx, turnevent.TurnComplete{FinalText: "done", Model: "claude-opus-5"})
		flush(t)
		root := findSpan(exp.GetSpans(), "turn")
		if root == nil {
			t.Fatal("no root span")
		}
		msg := eventAttr(t, root, 0, "retry", "error").AsString()
		if !strings.Contains(msg, "[REDACTED]") {
			t.Error(`retry event "error" attribute shows no [REDACTED] with content=false — the field must still be scrubbed`)
		}
		if strings.Contains(msg, secret) {
			t.Error(`retry event "error" attribute leaks the secret value with content=false`)
		}
	})

	t.Run("complete error fields", func(t *testing.T) {
		exp := setupTest(t, newOpts())
		sink, turn := NewTurnSink(turnevent.NopSink{})
		turn.Begin(TurnInfo{
			TurnID:     "agent/nccomp@1700000000000000000",
			SessionKey: "agent/nccomp",
			AgentID:    "agent",
			Trigger:    "test",
			Via:        "test",
			Backend:    "api",
			StartedAt:  time.Unix(0, 1700000000000000000),
		})
		sink.Emit(ctx, turnevent.TurnComplete{
			FinalText: "done",
			Model:     "claude-opus-5",
			Err:       errors.New("provider error quoting token " + secret),
		})
		flush(t)
		root := findSpan(exp.GetSpans(), "turn")
		if root == nil {
			t.Fatal("no root span")
		}
		status := attrStr(t, root.Attributes, attrObsStatus)
		if !strings.Contains(status, "[REDACTED]") {
			t.Error("langfuse.observation.status_message shows no [REDACTED] with content=false — the field must still be scrubbed")
		}
		if strings.Contains(status, secret) {
			t.Error("langfuse.observation.status_message leaks the secret value with content=false")
		}
		desc := root.Status.Description
		if !strings.Contains(desc, "[REDACTED]") {
			t.Error("root span status description shows no [REDACTED] with content=false — the field must still be scrubbed")
		}
		if strings.Contains(desc, secret) {
			t.Error("root span status description leaks the secret value with content=false")
		}
		if status != desc {
			t.Error("langfuse.observation.status_message and the span status description differ with content=false — both must carry the same redacted error text")
		}
	})

	t.Run("subagent label", func(t *testing.T) {
		exp := setupTest(t, newOpts())
		const turnID = "agent/ncsub@1700000000000000000"
		sink, turn := NewTurnSink(turnevent.NopSink{})
		turn.Begin(TurnInfo{
			TurnID:     turnID,
			SessionKey: "agent/ncsub",
			AgentID:    "agent",
			Trigger:    "test",
			Via:        "test",
			Backend:    "claude-code",
			StartedAt:  time.Unix(0, 1700000000000000000),
		})
		sink.Emit(ctx, turnevent.SubagentStart{
			GroupKey: "toolu_nc",
			Label:    "grep for " + secret,
			RunIndex: 1,
			Prompt:   "go",
		})
		sink.Emit(ctx, turnevent.SubagentEnd{GroupKey: "toolu_nc", RunIndex: 1})
		sink.Emit(ctx, turnevent.TurnComplete{FinalText: "done", Model: "claude-opus-5"})
		flush(t)
		sub := spanByID(exp.GetSpans(), SubagentSpanID(turnID, "toolu_nc", 1))
		if sub == nil {
			t.Fatal("no span with the expected subagent span id")
		}
		lbl := attrStr(t, sub.Attributes, attrObsMetaPrefix+"label")
		if !strings.Contains(lbl, "[REDACTED]") {
			t.Error("subagent label metadata attribute shows no [REDACTED] with content=false — the field must still be scrubbed")
		}
		if strings.Contains(lbl, secret) {
			t.Error("subagent label metadata attribute leaks the secret value with content=false")
		}
		if strings.Contains(sub.Name, secret) {
			t.Error("subagent span name leaks the secret value with content=false")
		}
	})
}

// TestCompleteErrorGenericPatternRedacted proves the error path applies the
// generic credential patterns too, not just known values: with NO secret
// values configured at all, an error quoting a bearer token is still
// scrubbed in the status message.
func TestCompleteErrorGenericPatternRedacted(t *testing.T) {
	exp := setupTest(t, Options{
		Content:       true,
		MaxFieldBytes: 1 << 20, // SecretValues left nil
	})
	sink, turn := NewTurnSink(turnevent.NopSink{})
	turn.Begin(TurnInfo{
		TurnID:     "agent/gen@1700000000000000000",
		SessionKey: "agent/gen",
		AgentID:    "agent",
		Trigger:    "test",
		Via:        "test",
		Backend:    "api",
		StartedAt:  time.Unix(0, 1700000000000000000),
	})
	token := "Bearer " + strings.Repeat("b", 20)
	sink.Emit(context.Background(), turnevent.TurnComplete{
		FinalText: "done",
		Model:     "claude-opus-5",
		Err:       errors.New("auth failed for " + token),
	})
	flush(t)

	root := findSpan(exp.GetSpans(), "turn")
	if root == nil {
		t.Fatal("no root span")
	}
	status := attrStr(t, root.Attributes, attrObsStatus)
	if !strings.Contains(status, "[REDACTED]") {
		t.Error("langfuse.observation.status_message shows no [REDACTED] — the generic bearer pattern was not applied to the error text")
	}
	if strings.Contains(status, token) {
		t.Error("langfuse.observation.status_message still holds the raw bearer credential quoted by the error")
	}
}

// TestCompleteErrorLiveValueRedacted proves the error path refreshes the
// redactor from the live secret-value function, exactly like the content
// fields: a value that only enters the function's result AFTER initWith is
// scrubbed from a later turn error.
func TestCompleteErrorLiveValueRedacted(t *testing.T) {
	vals := []string{"startuperrval1"}
	exp := setupTest(t, Options{
		Content:       true,
		MaxFieldBytes: 1 << 20,
		SecretValues:  func() []string { return vals },
	})

	// The secret reaches the value function only AFTER Init — a list cached
	// at Init cannot know it; a per-field read must.
	vals = append(vals, "liveerrval001")

	sink, turn := NewTurnSink(turnevent.NopSink{})
	turn.Begin(TurnInfo{
		TurnID:     "agent/liveerr@1700000000000000000",
		SessionKey: "agent/liveerr",
		AgentID:    "agent",
		Trigger:    "test",
		Via:        "test",
		Backend:    "api",
		StartedAt:  time.Unix(0, 1700000000000000000),
	})
	sink.Emit(context.Background(), turnevent.TurnComplete{
		FinalText: "done",
		Model:     "claude-opus-5",
		Err:       errors.New("upstream echoed authorization value liveerrval001"),
	})
	flush(t)

	root := findSpan(exp.GetSpans(), "turn")
	if root == nil {
		t.Fatal("no root span")
	}
	status := attrStr(t, root.Attributes, attrObsStatus)
	if !strings.Contains(status, "[REDACTED]") {
		t.Error("langfuse.observation.status_message shows no [REDACTED] — the late-added secret value was not scrubbed from the error text")
	}
	if strings.Contains(status, "liveerrval001") {
		t.Error("langfuse.observation.status_message leaks the secret value that entered the value function only after Init — the error path must read the function per field, not a copy")
	}
}
