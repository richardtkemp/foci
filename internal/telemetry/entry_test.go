package telemetry

import (
	"strings"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/modelinfo"
)

// TestEntryWithoutTurnID covers a helper call booked on a turn of its own
// (compaction, summariser, spawn): that turn is ledger bookkeeping, never a
// traced turn, so the call becomes its own single-observation trace named
// after its call type, keyed by RowTraceID, rather than hanging off a root
// span that does not exist.
func TestEntryWithoutTurnID(t *testing.T) {
	exp := setupTest(t, Options{Content: true, MaxFieldBytes: 1 << 20})

	ts := time.Unix(1700000000, 0)
	accounting.BookedHook(accounting.Booking{
		Call: accounting.Call{
			Backend: accounting.BackendAPI, Kind: accounting.KindCompaction,
			Session: "agent/c1", TurnID: "agent/c1@1700000000000000000:compaction",
			Model: "claude-opus-5", BilledAt: ts,
		},
		TurnSource: accounting.SourceCompaction,
		Instalment: true,
	})
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

// TestDirectAPICallHangsUnderItsTurn: a direct-API turn's own call is booked
// on the conversation turn the tracer opened, so its generation parents onto
// that turn's root span in the turn's trace, named as the pre-ledger rows
// were ("conversation") and carrying the ledger's per-call facts.
func TestDirectAPICallHangsUnderItsTurn(t *testing.T) {
	exp := setupTest(t, Options{Content: true, MaxFieldBytes: 1 << 20})

	const turnID = "gil/c1@1700000000000000000"
	cost := 0.02
	accounting.BookedHook(accounting.Booking{
		Call: accounting.Call{
			Key: "msg_1", Backend: accounting.BackendAPI, Kind: accounting.KindCall,
			Session: "gil/c1", AgentID: "gil", TurnID: turnID, Model: "claude-opus-5",
			BilledAt: time.Unix(1700000001, 0),
			Tokens:   modelinfo.Tokens{modelinfo.ClassInput: 10, modelinfo.ClassCacheWrite1h: 30},
			Detail:   map[string]any{"duration_ms": int64(900)},
		},
		CostUSD:    &cost,
		Fill:       40,
		TurnSource: accounting.SourceUser,
	})
	flush(t)

	sp := findSpan(exp.GetSpans(), "conversation")
	if sp == nil {
		t.Fatal("no generation span named conversation")
	}
	if sp.SpanContext.TraceID() != TraceIDForTurn(turnID) || sp.Parent.SpanID() != RootSpanID(turnID) {
		t.Errorf("generation is in trace %s under %s, want the turn's trace under its root",
			sp.SpanContext.TraceID(), sp.Parent.SpanID())
	}
	if got := attrStr(t, sp.Attributes, attrObsMetaPrefix+"token_scope"); got != "call" {
		t.Errorf("token_scope = %q, want call", got)
	}
	if got := attrStr(t, sp.Attributes, attrObsMetaPrefix+"call_key"); got != "msg_1" {
		t.Errorf("call_key = %q, want msg_1", got)
	}
	if !strings.Contains(attrStr(t, sp.Attributes, attrObsUsage), `"cache_creation_input_tokens":30`) {
		t.Errorf("usage = %s, want the 1h writes as cache creation", attrStr(t, sp.Attributes, attrObsUsage))
	}
	if d := sp.EndTime.Sub(sp.StartTime); d != 900*time.Millisecond {
		t.Errorf("span lasts %v, want the call's 900ms", d)
	}
}

// TestHelperCallOfATurnHangsUnderIt: a summary (or spawn, or compaction) that
// a conversation turn made is booked on that turn, so its generation joins
// the turn's trace under its root instead of a trace of its own.
func TestHelperCallOfATurnHangsUnderIt(t *testing.T) {
	exp := setupTest(t, Options{Content: true, MaxFieldBytes: 1 << 20})

	const turnID = "gil/c1@1700000000000000000"
	accounting.BookedHook(accounting.Booking{
		Call: accounting.Call{
			Key: "msg_s", Backend: accounting.BackendAPI, Kind: accounting.KindSummary,
			Session: "gil/c1", AgentID: "gil", TurnID: turnID, Model: "claude-haiku-4-5",
			BilledAt: time.Unix(1700000002, 0),
		},
		TurnSource: accounting.SourceUser,
	})
	flush(t)

	sp := findSpan(exp.GetSpans(), "summary")
	if sp == nil {
		t.Fatal("no generation span named summary")
	}
	if sp.SpanContext.TraceID() != TraceIDForTurn(turnID) || sp.Parent.SpanID() != RootSpanID(turnID) {
		t.Errorf("summary is in trace %s under %s, want its turn's trace under the root",
			sp.SpanContext.TraceID(), sp.Parent.SpanID())
	}
	if hasAttr(sp.Attributes, attrTraceName) {
		t.Error("a helper call of a traced turn must not name a trace of its own")
	}
}
