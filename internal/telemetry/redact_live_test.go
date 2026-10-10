package telemetry

import (
	"context"
	"strings"
	"testing"
	"time"

	"foci/internal/turnevent"
)

// TestLiveSecretValuesAddedAfterInitAreScrubbed proves the value function is
// read per exported field, not cached at Init: a value that only enters the
// function's result AFTER initWith is still scrubbed from the next exported
// field (SetInput's prompt → langfuse.observation.input). On the pre-#2249
// code this fails: Init copied a slice and never saw the value.
func TestLiveSecretValuesAddedAfterInitAreScrubbed(t *testing.T) {
	// Made-up values: >= 8 chars, lower-case letters and digits only, so
	// only value-based redaction can scrub them (no generic pattern hits).
	vals := []string{"startupval01"}
	exp := setupTest(t, Options{
		Content:       true,
		MaxFieldBytes: 1 << 20,
		SecretValues:  func() []string { return vals },
	})

	// The secret reaches the store only AFTER Init — a list cached at Init
	// cannot know it; a per-field read must.
	vals = append(vals, "liveadded01")

	sink, turn := NewTurnSink(turnevent.NopSink{})
	turn.Begin(TurnInfo{
		TurnID:     "agent/live@1700000000000000000",
		SessionKey: "agent/live",
		AgentID:    "agent",
		Trigger:    "test",
		Via:        "test",
		Backend:    "api",
		StartedAt:  time.Unix(0, 1700000000000000000),
	})
	turn.SetInput("prompt markerword liveadded01", "claude-opus-5")
	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "done", Model: "claude-opus-5"})
	flush(t)

	root := findSpan(exp.GetSpans(), "turn")
	if root == nil {
		t.Fatal("no root span")
	}
	in := attrStr(t, root.Attributes, attrObsInput)
	if !strings.Contains(in, "[REDACTED]") {
		t.Error("langfuse.observation.input shows no [REDACTED] — secret \"live.added\" was not scrubbed at all")
	}
	if strings.Contains(in, "liveadded01") {
		t.Error(`langfuse.observation.input leaks secret "live.added" — it entered the value function's result only after Init, so a cached-at-Init value list cannot scrub it`)
	}
}

// TestNilSecretValuesFuncAppliesGenericPatternsOnly proves a nil value
// function means "no secret values": only the generic credential patterns
// apply (here a bearer token) and nothing panics — reaching the assertions
// is the no-panic proof, since field runs on this path.
func TestNilSecretValuesFuncAppliesGenericPatternsOnly(t *testing.T) {
	exp := setupTest(t, Options{
		Content:       true,
		MaxFieldBytes: 1 << 20, // SecretValues left nil
	})

	sink, turn := NewTurnSink(turnevent.NopSink{})
	turn.Begin(TurnInfo{
		TurnID:     "agent/nil@1700000000000000000",
		SessionKey: "agent/nil",
		AgentID:    "agent",
		Trigger:    "test",
		Via:        "test",
		Backend:    "api",
		StartedAt:  time.Unix(0, 1700000000000000000),
	})
	token := "Bearer " + strings.Repeat("a", 24)
	turn.SetInput("prompt markerword Authorization: "+token, "claude-opus-5")
	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "done", Model: "claude-opus-5"})
	flush(t)

	root := findSpan(exp.GetSpans(), "turn")
	if root == nil {
		t.Fatal("no root span")
	}
	in := attrStr(t, root.Attributes, attrObsInput)
	if !strings.Contains(in, "[REDACTED]") {
		t.Error("langfuse.observation.input shows no [REDACTED] — the generic bearer pattern was not applied with a nil value function")
	}
	if strings.Contains(in, token) {
		t.Error("langfuse.observation.input still holds the raw bearer credential — the generic patterns must run with a nil value function")
	}
	if !strings.Contains(in, "markerword") {
		t.Error("langfuse.observation.input lost the plain marker word — a nil value function must not mean \"redact everything\"")
	}
}

// TestRemovedSecretValueStaysScrubbed is a characterisation test (listed in
// .factory/red-gate-exempt.txt: it also passes on the pre-#2249 code, whose
// startup list never changes). It pins the never-shrinking union: a value
// that was live for one exported field and then REMOVED from the value
// function's result stays scrubbed from a later exported field — a
// rotated-out credential can still appear in output.
func TestRemovedSecretValueStaysScrubbed(t *testing.T) {
	vals := []string{"rotatedout01"}
	exp := setupTest(t, Options{
		Content:       true,
		MaxFieldBytes: 1 << 20,
		SecretValues:  func() []string { return vals },
	})

	sink, turn := NewTurnSink(turnevent.NopSink{})
	turn.Begin(TurnInfo{
		TurnID:     "agent/gone@1700000000000000000",
		SessionKey: "agent/gone",
		AgentID:    "agent",
		Trigger:    "test",
		Via:        "test",
		Backend:    "api",
		StartedAt:  time.Unix(0, 1700000000000000000),
	})
	turn.SetInput("prompt rotatedout01", "claude-opus-5") // the union records the value here
	vals = nil                                            // then the store loses it
	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "done rotatedout01", Model: "claude-opus-5"})
	flush(t)

	root := findSpan(exp.GetSpans(), "turn")
	if root == nil {
		t.Fatal("no root span")
	}
	out := attrStr(t, root.Attributes, attrObsOutput)
	if !strings.Contains(out, "[REDACTED]") {
		t.Error("langfuse.observation.output shows no [REDACTED] — secret \"custom.rotated\" was not scrubbed at all")
	}
	if strings.Contains(out, "rotatedout01") {
		t.Error(`langfuse.observation.output leaks secret "custom.rotated" after its removal from the store — every value seen since startup must stay scrubbed`)
	}
}

// TestStartupValueRemovedBeforeFirstFieldStaysScrubbed is a characterisation
// test (listed in .factory/red-gate-exempt.txt: on the pre-#2249 code the
// startup slice kept the value too). It pins that the union is SEEDED at
// Init, not filled lazily at the first exported field: a startup value
// removed from the value function's result BEFORE any export is still
// scrubbed from the first field.
func TestStartupValueRemovedBeforeFirstFieldStaysScrubbed(t *testing.T) {
	vals := []string{"startupval01"}
	exp := setupTest(t, Options{
		Content:       true,
		MaxFieldBytes: 1 << 20,
		SecretValues:  func() []string { return vals },
	})

	vals = nil // removed from the store before ANY export

	sink, turn := NewTurnSink(turnevent.NopSink{})
	turn.Begin(TurnInfo{
		TurnID:     "agent/seed@1700000000000000000",
		SessionKey: "agent/seed",
		AgentID:    "agent",
		Trigger:    "test",
		Via:        "test",
		Backend:    "api",
		StartedAt:  time.Unix(0, 1700000000000000000),
	})
	turn.SetInput("prompt startupval01", "claude-opus-5")
	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "done", Model: "claude-opus-5"})
	flush(t)

	root := findSpan(exp.GetSpans(), "turn")
	if root == nil {
		t.Fatal("no root span")
	}
	in := attrStr(t, root.Attributes, attrObsInput)
	if !strings.Contains(in, "[REDACTED]") {
		t.Error("langfuse.observation.input shows no [REDACTED] — secret \"custom.startup\" was not scrubbed at all")
	}
	if strings.Contains(in, "startupval01") {
		t.Error(`langfuse.observation.input leaks secret "custom.startup" — it was present at Init and must stay scrubbed even though the store has since lost it`)
	}
}
