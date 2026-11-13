package telemetry

import (
	"context"
	"sync"
	"testing"
	"time"

	"foci/internal/turnevent"
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

// fakeExpiry replaces subExpiry for one test: every armed expiry is recorded
// and fires only when the test calls fire. It is the test's clock.
type fakeExpiry struct {
	mu    sync.Mutex
	armed []*fakeTimer
}

type fakeTimer struct {
	d       time.Duration
	fn      func()
	stopped bool
}

func useFakeExpiry(t *testing.T) *fakeExpiry {
	t.Helper()
	fe := &fakeExpiry{}
	old := subExpiry
	subExpiry = func(d time.Duration, fn func()) func() bool {
		ft := &fakeTimer{d: d, fn: fn}
		fe.mu.Lock()
		fe.armed = append(fe.armed, ft)
		fe.mu.Unlock()
		return func() bool {
			fe.mu.Lock()
			defer fe.mu.Unlock()
			was := !ft.stopped
			ft.stopped = true
			return was
		}
	}
	t.Cleanup(func() { subExpiry = old })
	return fe
}

func (fe *fakeExpiry) timers() []*fakeTimer {
	fe.mu.Lock()
	defer fe.mu.Unlock()
	return append([]*fakeTimer(nil), fe.armed...)
}

func openRuns() int {
	subMu.Lock()
	defer subMu.Unlock()
	return len(subRuns)
}

// TestSubagentLostEndExpiresWithoutLaterStart (#3075, #2968 F22): a run whose
// end event is lost must be closed after subagentTTL even when no other
// subagent ever starts. Before, only the next SubagentStart pruned stale runs,
// so the run's texts stayed in memory and its span stayed open until restart.
func TestSubagentLostEndExpiresWithoutLaterStart(t *testing.T) {
	exp := setupTest(t, Options{Content: true, MaxFieldBytes: 1 << 20})
	fe := useFakeExpiry(t)
	ctx := context.Background()

	sink, tn := NewTurnSink(turnevent.NopSink{})
	tn.Begin(TurnInfo{TurnID: "agentZ/s1@1", SessionKey: "agentZ/s1", AgentID: "agentZ", Trigger: "telegram", Via: "telegram", Backend: "claude-code", StartedAt: time.Now()})
	sink.Emit(ctx, turnevent.SubagentStart{GroupKey: "toolu_lost", Label: "lost", RunIndex: 1, Prompt: "p"})
	sink.Emit(ctx, turnevent.SubagentText{GroupKey: "toolu_lost", Text: "partial output", RunIndex: 1})
	tn.Complete("done", "model", nil, 0, nil)

	timers := fe.timers()
	if len(timers) != 1 {
		t.Fatalf("armed expiries = %d, want 1 per open run", len(timers))
	}
	if timers[0].d != subagentTTL {
		t.Errorf("expiry = %v, want subagentTTL %v", timers[0].d, subagentTTL)
	}
	if openRuns() != 1 {
		t.Fatalf("open runs before expiry = %d, want 1", openRuns())
	}

	timers[0].fn() // the TTL passes; no end event, no later subagent start

	if n := openRuns(); n != 0 {
		t.Errorf("open runs after expiry = %d, want 0 (the run leaked)", n)
	}
	flush(t)
	sub := findSpan(exp.GetSpans(), "subagent: lost")
	if sub == nil {
		t.Fatal("expired run's span was never ended (not exported)")
	}
	if !attrBool(t, sub.Attributes, attrObsMetaPrefix+"end_unobserved") {
		t.Error("expired run's span lacks end_unobserved=true")
	}
	if got := attrStr(t, sub.Attributes, attrObsOutput); got != "partial output" {
		t.Errorf("expired run output = %q, want the text collected so far", got)
	}
}

// TestSubagentEndDisarmsExpiry: a run that ends normally disarms its expiry,
// and a late-firing expiry for it touches nothing — not even a new run that
// reused the same key.
func TestSubagentEndDisarmsExpiry(t *testing.T) {
	exp := setupTest(t, Options{Content: true, MaxFieldBytes: 1 << 20})
	fe := useFakeExpiry(t)
	ctx := context.Background()

	sink, tn := NewTurnSink(turnevent.NopSink{})
	tn.Begin(TurnInfo{TurnID: "agentZ/s2@1", SessionKey: "agentZ/s2", AgentID: "agentZ", Trigger: "telegram", Via: "telegram", Backend: "claude-code", StartedAt: time.Now()})
	sink.Emit(ctx, turnevent.SubagentStart{GroupKey: "toolu_ok", Label: "ok", RunIndex: 1, Prompt: "p"})
	sink.Emit(ctx, turnevent.SubagentEnd{GroupKey: "toolu_ok", RunIndex: 1})
	sink.Emit(ctx, turnevent.SubagentStart{GroupKey: "toolu_ok", Label: "again", RunIndex: 1, Prompt: "p2"})

	timers := fe.timers()
	if len(timers) != 2 {
		t.Fatalf("armed expiries = %d, want 2", len(timers))
	}
	if !timers[0].stopped {
		t.Error("SubagentEnd did not disarm the run's expiry")
	}
	timers[0].fn() // a stale expiry that raced the end

	if n := openRuns(); n != 1 {
		t.Fatalf("open runs = %d, want 1: a stale expiry closed the newer run", n)
	}
	tn.Complete("done", "model", nil, 0, nil)
	flush(t)
	if sub := findSpan(exp.GetSpans(), "subagent: ok"); sub == nil || hasAttr(sub.Attributes, attrObsMetaPrefix+"end_unobserved") {
		t.Error("normally ended run missing or marked end_unobserved")
	}
}
