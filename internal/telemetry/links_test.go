package telemetry

import (
	"testing"
	"time"

	"foci/internal/agent/turnevent"
)

// TestActiveAndLastTurn: ActiveTurnID tracks the in-flight turn on a session
// and clears on Complete; LastTurnID then holds it.
func TestActiveAndLastTurn(t *testing.T) {
	setupTest(t, Options{Content: true, MaxFieldBytes: 1 << 20})

	const session = "agentA/s1"
	const turnID = "agentA/s1@1"
	_, turn := NewTurnSink(turnevent.NopSink{})
	turn.Begin(TurnInfo{TurnID: turnID, SessionKey: session, AgentID: "agentA", Trigger: "telegram", Via: "telegram", Backend: "claude-code", StartedAt: time.Now()})

	if got := activeTurnID(session); got != turnID {
		t.Errorf("ActiveTurnID = %q, want %q", got, turnID)
	}

	turn.Complete("done", "model", nil, 0, nil)

	if got := activeTurnID(session); got != "" {
		t.Errorf("ActiveTurnID after Complete = %q, want \"\"", got)
	}
	if got := lastTurnID(session); got != turnID {
		t.Errorf("LastTurnID = %q, want %q", got, turnID)
	}
}

// TestLinkPendingConsumedBySessionNotify: LinkPending stages the caller's
// turn; the next injected turn (session_notify) on the target session picks
// it up as its parent.
func TestLinkPendingConsumedBySessionNotify(t *testing.T) {
	exp := setupTest(t, Options{Content: true, MaxFieldBytes: 1 << 20})

	const sessionA = "agentA/s1"
	const turnA = "agentA/s1@1"
	_, ta := NewTurnSink(turnevent.NopSink{})
	ta.Begin(TurnInfo{TurnID: turnA, SessionKey: sessionA, AgentID: "agentA", Trigger: "telegram", Via: "telegram", Backend: "claude-code", StartedAt: time.Now()})
	ta.Complete("done", "model", nil, 0, nil)

	const sessionB = "agentB/s1"
	LinkPending(sessionB, sessionA)

	const turnB = "agentB/s1@1"
	_, tb := NewTurnSink(turnevent.NopSink{})
	tb.Begin(TurnInfo{TurnID: turnB, SessionKey: sessionB, AgentID: "agentB", Trigger: "session_notify", Via: "session_notify", Backend: "claude-code", StartedAt: time.Now()})
	tb.Complete("done", "model", nil, 0, nil)

	flush(t)
	roots := findSpans(exp.GetSpans(), "turn")
	if len(roots) != 2 {
		t.Fatalf("got %d root spans, want 2", len(roots))
	}
	root := rootForTrace(roots, TraceIDForTurn(turnB))
	if root == nil {
		t.Fatal("no root span for turn B")
	}
	if got := attrStr(t, root.Attributes, attrObsMetaPrefix+"parent_turn_id"); got != turnA {
		t.Errorf("parent_turn_id = %q, want %q", got, turnA)
	}
	if got := attrStr(t, root.Attributes, attrObsMetaPrefix+"parent_trace_id"); got != TraceIDForTurn(turnA).String() {
		t.Errorf("parent_trace_id = %q, want %q", got, TraceIDForTurn(turnA).String())
	}
	tags := attrStrSlice(t, root.Attributes, attrTraceTags)
	if !containsStr(tags, "from:agentA") {
		t.Errorf("tags %v missing %q", tags, "from:agentA")
	}
}

// TestLinkPendingNotConsumedByOrdinaryTrigger: a plain "telegram" turn on the
// target session does NOT consume the pending link — it stays staged for a
// later session_notify turn.
func TestLinkPendingNotConsumedByOrdinaryTrigger(t *testing.T) {
	exp := setupTest(t, Options{Content: true, MaxFieldBytes: 1 << 20})

	const sessionA = "agentA/s1"
	const turnA = "agentA/s1@1"
	_, ta := NewTurnSink(turnevent.NopSink{})
	ta.Begin(TurnInfo{TurnID: turnA, SessionKey: sessionA, AgentID: "agentA", Trigger: "telegram", Via: "telegram", Backend: "claude-code", StartedAt: time.Now()})
	ta.Complete("done", "model", nil, 0, nil)

	const sessionB = "agentB/s1"
	LinkPending(sessionB, sessionA)

	const turnB1 = "agentB/s1@1"
	_, tb1 := NewTurnSink(turnevent.NopSink{})
	tb1.Begin(TurnInfo{TurnID: turnB1, SessionKey: sessionB, AgentID: "agentB", Trigger: "telegram", Via: "telegram", Backend: "claude-code", StartedAt: time.Now()})
	tb1.Complete("done", "model", nil, 0, nil)

	const turnB2 = "agentB/s1@2"
	_, tb2 := NewTurnSink(turnevent.NopSink{})
	tb2.Begin(TurnInfo{TurnID: turnB2, SessionKey: sessionB, AgentID: "agentB", Trigger: "session_notify", Via: "session_notify", Backend: "claude-code", StartedAt: time.Now()})
	tb2.Complete("done", "model", nil, 0, nil)

	flush(t)
	roots := findSpans(exp.GetSpans(), "turn")
	if len(roots) != 3 {
		t.Fatalf("got %d root spans, want 3", len(roots))
	}
	rootB1 := rootForTrace(roots, TraceIDForTurn(turnB1))
	if rootB1 == nil {
		t.Fatal("no root span for turn B1")
	}
	if hasAttr(rootB1.Attributes, attrObsMetaPrefix+"parent_turn_id") {
		t.Error("turn B1 (trigger=telegram) must NOT consume the pending link")
	}
	rootB2 := rootForTrace(roots, TraceIDForTurn(turnB2))
	if rootB2 == nil {
		t.Fatal("no root span for turn B2")
	}
	if got := attrStr(t, rootB2.Attributes, attrObsMetaPrefix+"parent_turn_id"); got != turnA {
		t.Errorf("turn B2 (trigger=session_notify) parent_turn_id = %q, want %q", got, turnA)
	}
}

// TestLinkPendingNoKnownTurnIsNoop: LinkPending for a session with no active
// or last turn stages nothing.
func TestLinkPendingNoKnownTurnIsNoop(t *testing.T) {
	setupTest(t, Options{Content: true, MaxFieldBytes: 1 << 20})

	LinkPending("target-session", "session-nobody-ever-began")
	if l := takePendingLink("target-session", "session_notify"); l != nil {
		t.Errorf("expected no pending link, got one from %q", l.FromSession)
	}
}

// TestActiveTurnIDDisabled: with tracing off, ActiveTurnID always reports "".
func TestActiveTurnIDDisabled(t *testing.T) {
	setupTest(t, Options{Content: true, MaxFieldBytes: 1 << 20})
	const session = "agentA/s1"
	const turnID = "agentA/s1@1"
	_, turn := NewTurnSink(turnevent.NopSink{})
	turn.Begin(TurnInfo{TurnID: turnID, SessionKey: session, AgentID: "agentA", Trigger: "telegram", Via: "telegram", Backend: "claude-code", StartedAt: time.Now()})

	resetForTest() // disables tracing

	if got := activeTurnID(session); got != "" {
		t.Errorf("ActiveTurnID with tracing disabled = %q, want \"\"", got)
	}
}

// activeTurnID / lastTurnID read the link registry the way production code
// does internally (LinkPending); exported accessors were removed as unused.
func activeTurnID(session string) string {
	if !Enabled() {
		return ""
	}
	linkMu.Lock()
	defer linkMu.Unlock()
	return active[session]
}

func lastTurnID(session string) string {
	if !Enabled() {
		return ""
	}
	linkMu.Lock()
	defer linkMu.Unlock()
	return last[session]
}
