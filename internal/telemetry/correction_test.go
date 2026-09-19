package telemetry

import (
	"testing"
	"time"

	"foci/internal/log"
	"foci/internal/modelinfo"
)

// TestCorrectionHook: a #1918 cost correction is recorded as a zero-cost
// "event" under the subagent's span, in the subagent turn's own trace — not
// as a re-sent generation, since the append-only sink can't move money by
// re-billing either side.
func TestCorrectionHook(t *testing.T) {
	exp := setupTest(t, Options{Content: true, MaxFieldBytes: 1 << 20})

	const subagentTurnID = "agent/sub@1700000000000000001"
	c := modelinfo.CostCorrection{
		BilledAt:       time.Unix(1700000100, 0),
		SubagentTurnID: subagentTurnID,
		AgentID:        "toolu_9",
		Model:          "claude-opus-5",
		CostUSD:        0.05,
	}
	log.CorrectionHook(c, "agent/parent@1700000000000000000")
	flush(t)

	sp := findSpan(exp.GetSpans(), "cost_correction")
	if sp == nil {
		t.Fatal("no span named cost_correction")
	}
	if got := attrStr(t, sp.Attributes, attrObsType); got != "event" {
		t.Errorf("%s = %q, want %q", attrObsType, got, "event")
	}
	wantTrace := TraceIDForTurn(subagentTurnID)
	if sp.SpanContext.TraceID() != wantTrace {
		t.Errorf("trace id = %s, want %s", sp.SpanContext.TraceID(), wantTrace)
	}
	wantParent := SubagentSpanID(subagentTurnID, "toolu_9", 1)
	if sp.Parent.SpanID() != wantParent {
		t.Errorf("parent span id = %s, want %s", sp.Parent.SpanID(), wantParent)
	}
	if got := attrFloat(t, sp.Attributes, attrObsMetaPrefix+"moved_usd"); got != 0.05 {
		t.Errorf("moved_usd = %v, want 0.05", got)
	}
	if hasAttr(sp.Attributes, attrObsCost) {
		t.Error("cost_correction must not carry cost_details — it moves zero net spend")
	}
}
