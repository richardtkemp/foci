package telemetry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/log"
	"foci/internal/modelinfo"
	"foci/internal/provider"
)

var errBoom = errors.New("boom: something went wrong")

func ptr[T any](v T) *T { return &v }

// TestFullTurn drives one turn through the sink exactly the way the agent
// does (Begin, SetInput, then the ordered event stream, ending in
// TurnComplete), plus the two api.db rows a delegated turn and its subagent
// write through the log hooks, and checks the whole resulting trace shape:
// one root, one tool span, one subagent span, two generations, all sharing
// one trace id, and nothing extra.
func TestFullTurn(t *testing.T) {
	const secret = "SECRETVALUE123"
	exp := setupTest(t, Options{
		Content:       true,
		SystemPrompt:  true,
		MaxFieldBytes: 1 << 20,
		SecretValues:  []string{secret},
	})

	const turnID = "agent/c1@1700000000000000000"
	started := time.Unix(0, 1700000000000000000)
	ctx := context.Background()

	sink, turn := NewTurnSink(turnevent.NopSink{})
	if turn == nil {
		t.Fatal("NewTurnSink returned nil *Turn while enabled")
	}
	turn.Begin(TurnInfo{
		TurnID:     turnID,
		SessionKey: "agent/c1",
		AgentID:    "agent",
		Trigger:    "telegram",
		Via:        "telegram",
		Backend:    "claude-code",
		StartedAt:  started,
	})
	turn.SetInput("prompt with "+secret, "claude-opus-5")

	sink.Emit(ctx, turnevent.ToolCall{ID: "toolu_1", Name: "Bash", Args: []byte(`{"command":"ls"}`)})
	sink.Emit(ctx, turnevent.ToolResult{ID: "toolu_1", Name: "Bash", Output: "ok"})
	sink.Emit(ctx, turnevent.SubagentStart{GroupKey: "toolu_2", Label: "explore", RunIndex: 1, Prompt: "go look"})
	sink.Emit(ctx, turnevent.SubagentText{GroupKey: "toolu_2", Text: "found it", RunIndex: 1})
	sink.Emit(ctx, turnevent.SubagentEnd{GroupKey: "toolu_2", RunIndex: 1})
	sink.Emit(ctx, turnevent.TextBlock{Text: "working…", Phase: turnevent.PhaseIntermediate})
	sink.Emit(ctx, turnevent.ThinkingDelta{Delta: "hmm"})
	sink.Emit(ctx, turnevent.TurnComplete{
		FinalText: "done",
		Model:     "claude-opus-5",
		Cost:      0.5,
		Usage:     &provider.Usage{InputTokens: 10, OutputTokens: 5},
	})

	log.APIHook(log.APIEntry{
		Timestamp:         started,
		Session:           "agent/c1",
		Model:             "claude-opus-5",
		CallType:          "delegated_turn",
		TurnID:            turnID,
		DurationMS:        1200,
		CalculatedCostUSD: ptr(0.4),
		Turn:              &modelinfo.TokenCounts{Input: 10, Output: 5},
	}, false)
	log.APIHook(log.APIEntry{
		Timestamp:         started,
		Session:           "agent/c1",
		Model:             "claude-opus-5",
		CallType:          "subagent_turn",
		TurnID:            turnID,
		AgentID:           "toolu_2",
		DurationMS:        300,
		CalculatedCostUSD: ptr(0.1),
	}, false)

	flush(t)
	spans := exp.GetSpans()

	wantTrace := TraceIDForTurn(turnID)
	for _, sp := range spans {
		if sp.SpanContext.TraceID() != wantTrace {
			t.Errorf("span %q has trace id %s, want %s (all spans of one turn must share the trace)",
				sp.Name, sp.SpanContext.TraceID(), wantTrace)
		}
	}

	// --- root "turn" span ---
	roots := findSpans(spans, "turn")
	if len(roots) != 1 {
		t.Fatalf("got %d spans named %q, want exactly 1", len(roots), "turn")
	}
	root := roots[0]
	if root.SpanContext.TraceID() != wantTrace {
		t.Errorf("root trace id = %s, want %s", root.SpanContext.TraceID(), wantTrace)
	}
	if root.SpanContext.SpanID() != RootSpanID(turnID) {
		t.Errorf("root span id = %s, want %s", root.SpanContext.SpanID(), RootSpanID(turnID))
	}
	if got := attrStr(t, root.Attributes, attrObsType); got != "agent" {
		t.Errorf("root %s = %q, want %q", attrObsType, got, "agent")
	}
	if got := attrStr(t, root.Attributes, attrTraceName); got != "turn" {
		t.Errorf("root %s = %q, want %q", attrTraceName, got, "turn")
	}
	if got := attrStr(t, root.Attributes, attrUserID); got != "agent" {
		t.Errorf("root %s = %q, want %q", attrUserID, got, "agent")
	}
	if got := attrStr(t, root.Attributes, attrSessionID); got != "agent/c1" {
		t.Errorf("root %s = %q, want %q", attrSessionID, got, "agent/c1")
	}
	tags := attrStrSlice(t, root.Attributes, attrTraceTags)
	if !containsStr(tags, "backend:claude-code") {
		t.Errorf("root tags %v missing %q", tags, "backend:claude-code")
	}
	in := attrStr(t, root.Attributes, attrObsInput)
	if !strings.Contains(in, "[REDACTED]") {
		t.Errorf("root input %q missing [REDACTED]", in)
	}
	if strings.Contains(in, secret) {
		t.Errorf("root input %q leaked the secret value", in)
	}
	if got := attrStr(t, root.Attributes, attrObsOutput); got != "done" {
		t.Errorf("root %s = %q, want %q", attrObsOutput, got, "done")
	}
	if got := attrStr(t, root.Attributes, attrObsMetaPrefix+"thinking"); got != "hmm" {
		t.Errorf("root thinking = %q, want %q", got, "hmm")
	}
	if got := attrInt(t, root.Attributes, attrObsMetaPrefix+"tool_calls"); got != 1 {
		t.Errorf("root tool_calls = %d, want 1", got)
	}
	if got := attrInt(t, root.Attributes, attrObsMetaPrefix+"subagent_runs"); got != 1 {
		t.Errorf("root subagent_runs = %d, want 1", got)
	}
	if got := attrFloat(t, root.Attributes, attrObsMetaPrefix+"cost_usd"); got != 0.5 {
		t.Errorf("root cost_usd = %v, want 0.5", got)
	}

	// --- tool span "Bash" ---
	tool := findSpan(spans, "Bash")
	if tool == nil {
		t.Fatal("no span named Bash")
	}
	if tool.Parent.SpanID() != RootSpanID(turnID) {
		t.Errorf("Bash parent = %s, want root %s", tool.Parent.SpanID(), RootSpanID(turnID))
	}
	if got := attrStr(t, tool.Attributes, attrObsType); got != "tool" {
		t.Errorf("Bash %s = %q, want %q", attrObsType, got, "tool")
	}
	if !hasAttr(tool.Attributes, attrObsInput) {
		t.Error("Bash missing input attr")
	}
	if !hasAttr(tool.Attributes, attrObsOutput) {
		t.Error("Bash missing output attr")
	}

	// --- subagent span "subagent: explore" ---
	sub := findSpan(spans, "subagent: explore")
	if sub == nil {
		t.Fatal("no span named \"subagent: explore\"")
	}
	if sub.Parent.SpanID() != RootSpanID(turnID) {
		t.Errorf("subagent parent = %s, want root %s", sub.Parent.SpanID(), RootSpanID(turnID))
	}
	if got := attrStr(t, sub.Attributes, attrObsType); got != "agent" {
		t.Errorf("subagent %s = %q, want %q", attrObsType, got, "agent")
	}
	if got := attrStr(t, sub.Attributes, attrObsOutput); got != "found it" {
		t.Errorf("subagent output = %q, want %q", got, "found it")
	}

	// --- generation "delegated_turn" ---
	gen := findSpan(spans, "delegated_turn")
	if gen == nil {
		t.Fatal("no span named delegated_turn")
	}
	if gen.Parent.SpanID() != RootSpanID(turnID) {
		t.Errorf("delegated_turn parent = %s, want root %s", gen.Parent.SpanID(), RootSpanID(turnID))
	}
	if got := attrStr(t, gen.Attributes, attrObsType); got != "generation" {
		t.Errorf("delegated_turn %s = %q, want %q", attrObsType, got, "generation")
	}
	wantModel := modelinfo.Normalize("claude-opus-5")
	if got := attrStr(t, gen.Attributes, attrObsModel); got != wantModel {
		t.Errorf("delegated_turn %s = %q, want %q", attrObsModel, got, wantModel)
	}
	if got := attrStr(t, gen.Attributes, attrObsCost); got != `{"total":0.4}` {
		t.Errorf("delegated_turn cost_details = %q, want %q", got, `{"total":0.4}`)
	}
	if got := attrStr(t, gen.Attributes, attrObsUsage); !strings.Contains(got, `"input":10`) {
		t.Errorf("delegated_turn usage_details = %q, want it to contain %q", got, `"input":10`)
	}

	// --- generation "subagent_turn" ---
	subGen := findSpan(spans, "subagent_turn")
	if subGen == nil {
		t.Fatal("no span named subagent_turn")
	}
	wantSubParent := SubagentSpanID(turnID, "toolu_2", 1)
	if subGen.Parent.SpanID() != wantSubParent {
		t.Errorf("subagent_turn parent = %s, want %s", subGen.Parent.SpanID(), wantSubParent)
	}

	// --- nothing extra leaked in ---
	if len(spans) != 5 {
		names := make([]string, len(spans))
		for i, sp := range spans {
			names[i] = sp.Name
		}
		t.Errorf("got %d spans %v, want exactly 5 (turn, Bash, subagent: explore, delegated_turn, subagent_turn)", len(spans), names)
	}
}

// TestTurnErrorAndEdgeCases covers Complete's error path and the handful of
// double-call / unseen-id edge cases the doc comments in turn.go call out.
func TestTurnErrorAndEdgeCases(t *testing.T) {
	t.Run("Complete with error sets ERROR level and status", func(t *testing.T) {
		exp := setupTest(t, Options{Content: true, MaxFieldBytes: 1 << 20})
		turnID := "agent/err@1"
		_, turn := NewTurnSink(turnevent.NopSink{})
		turn.Begin(TurnInfo{TurnID: turnID, SessionKey: "agent/err", AgentID: "agent", Trigger: "telegram", Via: "telegram", Backend: "claude-code", StartedAt: time.Now()})
		turn.Complete("", "claude-opus-5", nil, 0, errBoom)
		flush(t)
		root := findSpan(exp.GetSpans(), "turn")
		if root == nil {
			t.Fatal("no root span")
		}
		if got := attrStr(t, root.Attributes, attrObsLevel); got != "ERROR" {
			t.Errorf("level = %q, want ERROR", got)
		}
		if got := attrStr(t, root.Attributes, attrObsStatus); got != errBoom.Error() {
			t.Errorf("status = %q, want %q", got, errBoom.Error())
		}
	})

	t.Run("Begin with empty TurnID opens no span", func(t *testing.T) {
		exp := setupTest(t, Options{Content: true, MaxFieldBytes: 1 << 20})
		_, turn := NewTurnSink(turnevent.NopSink{})
		turn.Begin(TurnInfo{TurnID: "", SessionKey: "agent/empty", AgentID: "agent", Trigger: "telegram", Via: "telegram", Backend: "claude-code", StartedAt: time.Now()})
		turn.SetInput("prompt", "model")
		turn.Complete("done", "model", nil, 0, nil)
		flush(t)
		if spans := exp.GetSpans(); len(spans) != 0 {
			t.Errorf("got %d spans, want 0 for a turn that never began", len(spans))
		}
	})

	t.Run("Complete twice still yields one root span", func(t *testing.T) {
		exp := setupTest(t, Options{Content: true, MaxFieldBytes: 1 << 20})
		turnID := "agent/twice@1"
		_, turn := NewTurnSink(turnevent.NopSink{})
		turn.Begin(TurnInfo{TurnID: turnID, SessionKey: "agent/twice", AgentID: "agent", Trigger: "telegram", Via: "telegram", Backend: "claude-code", StartedAt: time.Now()})
		turn.Complete("first", "model", nil, 0, nil)
		turn.Complete("second", "model", nil, 0, nil)
		flush(t)
		roots := findSpans(exp.GetSpans(), "turn")
		if len(roots) != 1 {
			t.Fatalf("got %d root spans, want 1", len(roots))
		}
		if got := attrStr(t, roots[0].Attributes, attrObsOutput); got != "first" {
			t.Errorf("output = %q, want %q (second Complete must be a no-op)", got, "first")
		}
	})

	t.Run("ToolEnd for an unseen id still records a span", func(t *testing.T) {
		exp := setupTest(t, Options{Content: true, MaxFieldBytes: 1 << 20})
		turnID := "agent/unseen@1"
		_, turn := NewTurnSink(turnevent.NopSink{})
		turn.Begin(TurnInfo{TurnID: turnID, SessionKey: "agent/unseen", AgentID: "agent", Trigger: "telegram", Via: "telegram", Backend: "claude-code", StartedAt: time.Now()})
		turn.ToolEnd("toolu_ghost", "Bash", "out", false)
		turn.Complete("done", "model", nil, 0, nil)
		flush(t)
		sp := findSpan(exp.GetSpans(), "Bash")
		if sp == nil {
			t.Fatal("no Bash span for the unseen-id ToolEnd")
		}
		if !attrBool(t, sp.Attributes, attrObsMetaPrefix+"start_unobserved") {
			t.Error("expected start_unobserved=true")
		}
	})

	t.Run("a tool left open at Complete is ended unresolved", func(t *testing.T) {
		exp := setupTest(t, Options{Content: true, MaxFieldBytes: 1 << 20})
		turnID := "agent/open@1"
		sink, turn := NewTurnSink(turnevent.NopSink{})
		turn.Begin(TurnInfo{TurnID: turnID, SessionKey: "agent/open", AgentID: "agent", Trigger: "telegram", Via: "telegram", Backend: "claude-code", StartedAt: time.Now()})
		sink.Emit(context.Background(), turnevent.ToolCall{ID: "toolu_open", Name: "Bash", Args: []byte(`{}`)})
		turn.Complete("done", "model", nil, 0, nil)
		flush(t)
		sp := findSpan(exp.GetSpans(), "Bash")
		if sp == nil {
			t.Fatal("no Bash span for the tool left open")
		}
		if !attrBool(t, sp.Attributes, attrObsMetaPrefix+"end_unobserved") {
			t.Error("expected end_unobserved=true")
		}
	})
}

// TestContentOff checks the Content:false switch: no input/output text
// leaves the process, but the always-on char-count metadata still does.
func TestContentOff(t *testing.T) {
	exp := setupTest(t, Options{Content: false, MaxFieldBytes: 1 << 20})
	turnID := "agent/nocontent@1"
	sink, turn := NewTurnSink(turnevent.NopSink{})
	turn.Begin(TurnInfo{TurnID: turnID, SessionKey: "agent/nocontent", AgentID: "agent", Trigger: "telegram", Via: "telegram", Backend: "claude-code", StartedAt: time.Now()})
	turn.SetInput("a prompt", "model")
	sink.Emit(context.Background(), turnevent.ToolCall{ID: "toolu_1", Name: "Bash", Args: []byte(`{"command":"ls"}`)})
	sink.Emit(context.Background(), turnevent.ToolResult{ID: "toolu_1", Name: "Bash", Output: "ok"})
	turn.Complete("done", "model", nil, 0, nil)
	flush(t)
	spans := exp.GetSpans()

	root := findSpan(spans, "turn")
	if root == nil {
		t.Fatal("no root span")
	}
	if hasAttr(root.Attributes, attrObsInput) {
		t.Error("root has input attr with Content=false")
	}
	if hasAttr(root.Attributes, attrObsOutput) {
		t.Error("root has output attr with Content=false")
	}
	if !hasAttr(root.Attributes, attrObsMetaPrefix+"input_chars") {
		t.Error("root missing input_chars metadata")
	}

	tool := findSpan(spans, "Bash")
	if tool == nil {
		t.Fatal("no Bash span")
	}
	if hasAttr(tool.Attributes, attrObsInput) {
		t.Error("tool has input attr with Content=false")
	}
	if hasAttr(tool.Attributes, attrObsOutput) {
		t.Error("tool has output attr with Content=false")
	}
	if !hasAttr(tool.Attributes, attrObsMetaPrefix+"input_chars") {
		t.Error("tool missing input_chars metadata")
	}
}
