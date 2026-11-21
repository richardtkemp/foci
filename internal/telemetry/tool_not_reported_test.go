package telemetry

import (
	"context"
	"testing"
	"time"

	"foci/internal/turnevent"
)

// TestToolSpan_OutputNotReported (#3188): a tool result the backend did not
// report gives a span that says so (metadata output_not_reported, and the
// placeholder output text with content on), and a tool that really printed
// nothing keeps an empty output with no flag.
func TestToolSpan_OutputNotReported(t *testing.T) {
	exp := setupTest(t, Options{Content: true, MaxFieldBytes: 1 << 20})
	sink, turn := NewTurnSink(turnevent.NopSink{})
	turn.Begin(TurnInfo{TurnID: "agent/nr@1", SessionKey: "agent/nr", AgentID: "agent", Trigger: "app", Via: "app", Backend: "codex", StartedAt: time.Now()})
	ctx := context.Background()
	sink.Emit(ctx, turnevent.ToolCall{ID: "m1", Name: "mcp:s.t", Args: []byte(`{}`)})
	sink.Emit(ctx, turnevent.ToolResult{ID: "m1", Name: "mcp:s.t", OutputNotReported: true})
	sink.Emit(ctx, turnevent.ToolCall{ID: "c1", Name: "bash", Args: []byte(`{}`)})
	sink.Emit(ctx, turnevent.ToolResult{ID: "c1", Name: "bash", Output: ""})
	turn.Complete("done", "model", nil, 0, nil)
	flush(t)

	nr := findSpan(exp.GetSpans(), "mcp:s.t")
	if nr == nil {
		t.Fatal("no span for the not-reported tool")
	}
	if !attrBool(t, nr.Attributes, attrObsMetaPrefix+"output_not_reported") {
		t.Error("output_not_reported is not true on the not-reported tool's span")
	}
	if got := attrStr(t, nr.Attributes, attrObsOutput); got != toolOutputNotReported {
		t.Errorf("not-reported output = %q, want %q", got, toolOutputNotReported)
	}
	if hasAttr(nr.Attributes, attrObsMetaPrefix+"output_chars") {
		t.Error("a not-reported tool's span claims an output size")
	}

	empty := findSpan(exp.GetSpans(), "bash")
	if empty == nil {
		t.Fatal("no span for the empty-output tool")
	}
	if hasAttr(empty.Attributes, attrObsMetaPrefix+"output_not_reported") {
		t.Error("a tool that printed nothing is marked not reported")
	}
	if got := attrStr(t, empty.Attributes, attrObsOutput); got != "" {
		t.Errorf("empty-output tool's output = %q, want empty", got)
	}
}

// TestToolSpan_OutputNotReported_ContentOff: with content off the flag still
// reaches the span; no output text is recorded.
func TestToolSpan_OutputNotReported_ContentOff(t *testing.T) {
	exp := setupTest(t, Options{Content: false, MaxFieldBytes: 1 << 20})
	_, turn := NewTurnSink(turnevent.NopSink{})
	turn.Begin(TurnInfo{TurnID: "agent/nroff@1", SessionKey: "agent/nroff", AgentID: "agent", Trigger: "app", Via: "app", Backend: "codex", StartedAt: time.Now()})
	turn.ToolEnd("w1", "web_search", "", false, true)
	turn.Complete("done", "model", nil, 0, nil)
	flush(t)
	sp := findSpan(exp.GetSpans(), "web_search")
	if sp == nil {
		t.Fatal("no web_search span")
	}
	if !attrBool(t, sp.Attributes, attrObsMetaPrefix+"output_not_reported") {
		t.Error("output_not_reported is not true with content off")
	}
	if hasAttr(sp.Attributes, attrObsOutput) {
		t.Error("an output was recorded with content off")
	}
}
