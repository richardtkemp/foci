package telemetry

import (
	"testing"
	"time"

	"foci/internal/log"
)

// TestEntryWithoutTurnID covers a row that names no turn (compaction,
// summariser, spawn): it becomes its own single-observation trace named
// after its call type, keyed by RowTraceID, rather than hanging off any
// turn's root.
func TestEntryWithoutTurnID(t *testing.T) {
	exp := setupTest(t, Options{Content: true, MaxFieldBytes: 1 << 20})

	ts := time.Unix(1700000000, 0)
	entry := log.APIEntry{
		CallType:  "compaction",
		Session:   "agent/c1",
		Model:     "claude-opus-5",
		Timestamp: ts,
	}
	log.APIHook(entry, true)
	flush(t)

	sp := findSpan(exp.GetSpans(), "compaction")
	if sp == nil {
		t.Fatal("no span named compaction")
	}

	wantTrace := RowTraceID("agent/c1", "compaction", ts)
	if sp.SpanContext.TraceID() != wantTrace {
		t.Errorf("trace id = %s, want %s", sp.SpanContext.TraceID(), wantTrace)
	}
	if got := attrStr(t, sp.Attributes, attrTraceName); got != "compaction" {
		t.Errorf("%s = %q, want %q", attrTraceName, got, "compaction")
	}
	tags := attrStrSlice(t, sp.Attributes, attrTraceTags)
	if !containsStr(tags, "call_type:compaction") {
		t.Errorf("tags %v missing %q", tags, "call_type:compaction")
	}
	if !attrBool(t, sp.Attributes, attrObsMetaPrefix+"instalment") {
		t.Error("expected instalment=true metadata")
	}
}
