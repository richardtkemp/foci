package telemetry

import (
	"context"
	"testing"
	"time"

	"foci/internal/agent/turnevent"
)

// TestSubagentEndOnLaterTurn: a background subagent can outlive the turn
// that spawned it — its SubagentText/SubagentEnd events arrive on whichever
// turn's sink is live when they land. The resulting span must still belong
// to the SPAWNING turn's trace, parented on that turn's root, not the later
// turn's.
func TestSubagentEndOnLaterTurn(t *testing.T) {
	exp := setupTest(t, Options{Content: true, MaxFieldBytes: 1 << 20})
	ctx := context.Background()

	const session = "agentY/s1"
	const turn1 = "agentY/s1@1"
	sink1, t1 := NewTurnSink(turnevent.NopSink{})
	t1.Begin(TurnInfo{TurnID: turn1, SessionKey: session, AgentID: "agentY", Trigger: "telegram", Via: "telegram", Backend: "claude-code", StartedAt: time.Now()})
	sink1.Emit(ctx, turnevent.SubagentStart{GroupKey: "toolu_9", Label: "bg", RunIndex: 1, Prompt: "go do it in the background"})
	t1.Complete("first turn done", "model", nil, 0, nil)

	const turn2 = "agentY/s1@2"
	sink2, t2 := NewTurnSink(turnevent.NopSink{})
	t2.Begin(TurnInfo{TurnID: turn2, SessionKey: session, AgentID: "agentY", Trigger: "telegram", Via: "telegram", Backend: "claude-code", StartedAt: time.Now()})
	sink2.Emit(ctx, turnevent.SubagentText{GroupKey: "toolu_9", Text: "background result", RunIndex: 1})
	sink2.Emit(ctx, turnevent.SubagentEnd{GroupKey: "toolu_9", RunIndex: 1})
	t2.Complete("second turn done", "model", nil, 0, nil)

	flush(t)
	spans := exp.GetSpans()

	sub := findSpan(spans, "subagent: bg")
	if sub == nil {
		t.Fatal("no span named \"subagent: bg\"")
	}
	if got := sub.SpanContext.TraceID(); got != TraceIDForTurn(turn1) {
		t.Errorf("subagent trace id = %s, want turn1's trace %s", got, TraceIDForTurn(turn1))
	}
	if got := sub.Parent.SpanID(); got != RootSpanID(turn1) {
		t.Errorf("subagent parent = %s, want turn1's root %s", got, RootSpanID(turn1))
	}
	if got := attrStr(t, sub.Attributes, attrObsOutput); got != "background result" {
		t.Errorf("subagent output = %q, want %q", got, "background result")
	}
}
