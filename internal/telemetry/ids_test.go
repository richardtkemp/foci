package telemetry

import "testing"

// TestIDsDeterministic: same input, same role → same id, every time.
func TestIDsDeterministic(t *testing.T) {
	turnID := "agent/c1@1700000000000000000"
	toolID := "toolu_1"
	group := "toolu_2"

	if TraceIDForTurn(turnID) != TraceIDForTurn(turnID) {
		t.Error("TraceIDForTurn not deterministic")
	}
	if RootSpanID(turnID) != RootSpanID(turnID) {
		t.Error("RootSpanID not deterministic")
	}
	if ToolSpanID(turnID, toolID) != ToolSpanID(turnID, toolID) {
		t.Error("ToolSpanID not deterministic")
	}
	if SubagentSpanID(turnID, group, 1) != SubagentSpanID(turnID, group, 1) {
		t.Error("SubagentSpanID not deterministic")
	}
	if GenerationSpanID(turnID, "a", "b") != GenerationSpanID(turnID, "a", "b") {
		t.Error("GenerationSpanID not deterministic")
	}
}

// TestIDsDistinctAcrossInputs: different turn/tool/group inputs never collide.
func TestIDsDistinctAcrossInputs(t *testing.T) {
	if TraceIDForTurn("turn-a") == TraceIDForTurn("turn-b") {
		t.Error("TraceIDForTurn collided across distinct turn ids")
	}
	if RootSpanID("turn-a") == RootSpanID("turn-b") {
		t.Error("RootSpanID collided across distinct turn ids")
	}
	if ToolSpanID("turn-a", "tool-1") == ToolSpanID("turn-a", "tool-2") {
		t.Error("ToolSpanID collided across distinct tool ids")
	}
	if ToolSpanID("turn-a", "tool-1") == ToolSpanID("turn-b", "tool-1") {
		t.Error("ToolSpanID collided across distinct turn ids")
	}
	if SubagentSpanID("turn-a", "group-1", 1) == SubagentSpanID("turn-a", "group-2", 1) {
		t.Error("SubagentSpanID collided across distinct group keys")
	}
	if GenerationSpanID("turn-a", "x") == GenerationSpanID("turn-a", "y") {
		t.Error("GenerationSpanID collided across distinct parts")
	}
}

// TestIDsDistinctAcrossRoles: the same underlying turn id, run through the
// different role prefixes, never produces the same span id twice — the
// prefix salts the digest so a tool span can't collide with a subagent span,
// etc.
func TestIDsDistinctAcrossRoles(t *testing.T) {
	turnID := "agent/c1@1700000000000000000"
	seen := map[string]string{}
	check := func(role, id string) {
		if other, dup := seen[id]; dup {
			t.Errorf("span id collision between role %q and %q: %s", role, other, id)
		}
		seen[id] = role
	}
	check("root", RootSpanID(turnID).String())
	check("tool", ToolSpanID(turnID, "toolu_1").String())
	check("subagent", SubagentSpanID(turnID, "toolu_1", 1).String())
	check("generation", GenerationSpanID(turnID).String())
	check("event", EventSpanID(turnID).String())
}

// TestSubagentSpanIDRunCollapse: run 1 and run 0 collapse to the same span id
// (the initial spawn and a run count nobody bothered to set) while run 2 (a
// SendMessage reactivation) gets its own.
func TestSubagentSpanIDRunCollapse(t *testing.T) {
	turnID := "agent/c1@1700000000000000000"
	group := "toolu_2"

	run0 := SubagentSpanID(turnID, group, 0)
	run1 := SubagentSpanID(turnID, group, 1)
	run2 := SubagentSpanID(turnID, group, 2)

	if run0 != run1 {
		t.Errorf("SubagentSpanID(run=0) = %s, want equal to run=1 %s", run0, run1)
	}
	if run1 == run2 {
		t.Errorf("SubagentSpanID(run=1) = %s, want distinct from run=2 %s", run1, run2)
	}
}
