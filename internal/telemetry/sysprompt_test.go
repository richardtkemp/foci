package telemetry

import (
	"testing"
	"time"

	"foci/internal/turnevent"
)

// TestSystemPrompt covers the export-once-per-session-per-hash rule: the
// first turn after RegisterSystemPrompt gets the hash on its root AND a
// child "system_prompt" event span with the text; a second turn with the
// same prompt in force gets the hash only; re-registering different text
// produces one more "system_prompt" span on the next turn.
func TestSystemPrompt(t *testing.T) {
	exp := setupTest(t, Options{Content: true, SystemPrompt: true, MaxFieldBytes: 1 << 20})

	const session = "agentX/s1"
	RegisterSystemPrompt(session, "you are a helpful assistant", "launch")

	const turn1 = "agentX/s1@1"
	_, t1 := NewTurnSink(turnevent.NopSink{})
	t1.Begin(TurnInfo{TurnID: turn1, SessionKey: session, AgentID: "agentX", Trigger: "telegram", Via: "telegram", Backend: "claude-code", StartedAt: time.Now()})
	t1.Complete("done", "model", nil, 0, nil)

	const turn2 = "agentX/s1@2"
	_, t2 := NewTurnSink(turnevent.NopSink{})
	t2.Begin(TurnInfo{TurnID: turn2, SessionKey: session, AgentID: "agentX", Trigger: "telegram", Via: "telegram", Backend: "claude-code", StartedAt: time.Now()})
	t2.Complete("done", "model", nil, 0, nil)

	RegisterSystemPrompt(session, "you are a DIFFERENT assistant", "launch")

	const turn3 = "agentX/s1@3"
	_, t3 := NewTurnSink(turnevent.NopSink{})
	t3.Begin(TurnInfo{TurnID: turn3, SessionKey: session, AgentID: "agentX", Trigger: "telegram", Via: "telegram", Backend: "claude-code", StartedAt: time.Now()})
	t3.Complete("done", "model", nil, 0, nil)

	flush(t)
	spans := exp.GetSpans()
	roots := findSpans(spans, "turn")
	if len(roots) != 3 {
		t.Fatalf("got %d root spans, want 3", len(roots))
	}
	root1 := rootForTrace(roots, TraceIDForTurn(turn1))
	root2 := rootForTrace(roots, TraceIDForTurn(turn2))
	root3 := rootForTrace(roots, TraceIDForTurn(turn3))
	if root1 == nil || root2 == nil || root3 == nil {
		t.Fatal("missing a root span for one of the three turns")
	}

	hash1 := attrStr(t, root1.Attributes, attrObsMetaPrefix+"system_prompt_sha256")
	hash2 := attrStr(t, root2.Attributes, attrObsMetaPrefix+"system_prompt_sha256")
	hash3 := attrStr(t, root3.Attributes, attrObsMetaPrefix+"system_prompt_sha256")
	if hash1 == "" {
		t.Error("turn1 missing system_prompt_sha256")
	}
	if hash2 != hash1 {
		t.Errorf("turn2 hash = %q, want same as turn1 %q (prompt unchanged)", hash2, hash1)
	}
	if hash3 == hash1 {
		t.Errorf("turn3 hash = %q, want different from turn1 %q (prompt was re-registered)", hash3, hash1)
	}

	sysSpans := findSpans(spans, "system_prompt")
	if len(sysSpans) != 2 {
		t.Fatalf("got %d system_prompt spans, want 2 (one for the first hash, one for the new hash) — turn2 must not re-export", len(sysSpans))
	}
}

// TestSystemPromptTextOffButHashOn: with SystemPrompt:false the hash
// metadata is still recorded (it's cheap and useful for grouping) but the
// full text child span is never emitted.
func TestSystemPromptTextOffButHashOn(t *testing.T) {
	exp := setupTest(t, Options{Content: true, SystemPrompt: false, MaxFieldBytes: 1 << 20})

	const session = "agentZ/s1"
	RegisterSystemPrompt(session, "you are a helpful assistant", "launch")

	const turnID = "agentZ/s1@1"
	_, turn := NewTurnSink(turnevent.NopSink{})
	turn.Begin(TurnInfo{TurnID: turnID, SessionKey: session, AgentID: "agentZ", Trigger: "telegram", Via: "telegram", Backend: "claude-code", StartedAt: time.Now()})
	turn.Complete("done", "model", nil, 0, nil)

	flush(t)
	spans := exp.GetSpans()
	root := findSpan(spans, "turn")
	if root == nil {
		t.Fatal("no root span")
	}
	if got := attrStr(t, root.Attributes, attrObsMetaPrefix+"system_prompt_sha256"); got == "" {
		t.Error("expected system_prompt_sha256 metadata even with SystemPrompt:false")
	}
	if sp := findSpan(spans, "system_prompt"); sp != nil {
		t.Error("system_prompt text span must not be emitted with SystemPrompt:false")
	}
}
