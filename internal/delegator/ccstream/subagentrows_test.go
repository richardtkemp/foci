package ccstream

import (
	"math"
	"testing"

	"foci/internal/delegator"
	"foci/internal/modelinfo"
)

// fullMsg builds an assistant message with every token class set, so a test can
// check the whole split rather than cache writes alone. parent != "" makes it a
// SUBAGENT's message (ParentToolUseID is what the accumulator buckets on).
func fullMsg(model, id, parent string, in, out, cr, cw5m, cw1h int) *AssistantMessage {
	m := &AssistantMessage{}
	m.Message.ID = id
	m.Message.Model = model
	m.Message.Usage = TokenUsage{
		InputTokens:              in,
		OutputTokens:             out,
		CacheReadInputTokens:     cr,
		CacheCreationInputTokens: cw5m + cw1h,
	}
	if cw5m > 0 || cw1h > 0 {
		m.Message.Usage.CacheCreation = &CacheCreationSplit{Ephemeral5m: cw5m, Ephemeral1h: cw1h}
	}
	if parent != "" {
		m.ParentToolUseID = &parent
	}
	return m
}

// TestOnResult_SubagentSpendLeavesTheParentRow is #1880 phase C.
//
// Before it, a subagent's spend was booked to whichever parent turn happened to
// close while the subagent was running: one measured 3.5-minute turn carried 34
// minutes of someone else's work and $11.71, which defeats the obvious
// diagnostic — an expensive turn looks like an expensive turn.
//
// The turn here is an opus parent whose cache writes are 1h, plus a sonnet
// subagent whose writes are 5m. That combination is the one that matters: the
// two models have different rates AND different TTLs, so a split that leaked
// either dimension across the boundary lands on a different number.
func TestOnResult_SubagentSpendLeavesTheParentRow(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	b.noteAssistantUsage(fullMsg("claude-opus-5", "msg_top", "", 10, 1000, 5000, 0, 40000))
	b.noteAssistantUsage(fullMsg("claude-sonnet-5", "msg_sub", "agent-x", 4, 2000, 7000, 100000, 0))

	got := onResultWith(b, map[string]ModelUsage{
		"claude-opus-5":   {InputTokens: 10, OutputTokens: 1000, CacheReadInputTokens: 5000, CacheCreationInputTokens: 40000, CostUSD: 0.4},
		"claude-sonnet-5": {InputTokens: 4, OutputTokens: 2000, CacheReadInputTokens: 7000, CacheCreationInputTokens: 100000, CostUSD: 0.3},
	})
	if got == nil || got.Usage == nil || got.Usage.CalculatedCostUSD == nil {
		t.Fatal("no calculated cost")
	}

	// --- the parent row ---
	// opus only: 10 in @$5, 1000 out @$25, 5000 read @$0.50, 40000 write @$10
	// (1h). If the sonnet subagent had leaked in, every one of these is wrong.
	wantParent := 10*5.0/1e6 + 1000*25.0/1e6 + 5000*0.5/1e6 + 40000*10.0/1e6
	if math.Abs(*got.Usage.CalculatedCostUSD-wantParent) > 1e-9 {
		t.Errorf("parent cost = %.9f, want %.9f — the parent row must carry its own work only",
			*got.Usage.CalculatedCostUSD, wantParent)
	}
	wantParentCounts := modelinfo.TokenCounts{Input: 10, Output: 1000, CacheRead: 5000, CacheWrite: 40000}
	if got.Usage.Turn == nil || *got.Usage.Turn != wantParentCounts {
		t.Errorf("parent counts = %+v, want %+v", got.Usage.Turn, wantParentCounts)
	}

	// --- the subagent row ---
	if len(got.Usage.Subagents) != 1 {
		t.Fatalf("Subagents = %+v, want exactly one", got.Usage.Subagents)
	}
	sc := got.Usage.Subagents[0]
	if sc.AgentID != "agent-x" {
		t.Errorf("AgentID = %q, want agent-x — the row has to name the work that incurred it", sc.AgentID)
	}
	if sc.Model != "claude/claude-sonnet-5" {
		t.Errorf("Model = %q, want claude/claude-sonnet-5 — priced at the model that ran, not the parent's", sc.Model)
	}
	// sonnet: 4 in @$2, 2000 out @$10, 7000 read @$0.20, 100000 write @$2.50
	// (5m). At sonnet's 1h rate ($4) the write alone would be $0.40, not $0.25.
	wantSub := 4*2.0/1e6 + 2000*10.0/1e6 + 7000*0.2/1e6 + 100000*2.5/1e6
	if math.Abs(sc.CostUSD-wantSub) > 1e-9 {
		t.Errorf("subagent cost = %.9f, want %.9f", sc.CostUSD, wantSub)
	}

	// --- conservation: every token emitted exactly once ---
	// This is the invariant the split has to preserve, and the one a plausible
	// implementation breaks silently: parent + subagents must reconstruct the
	// authoritative ModelUsage totals, class by class, with nothing dropped and
	// nothing counted twice.
	sum := *got.Usage.Turn
	for _, s := range got.Usage.Subagents {
		sum = sum.Add(s.Counts)
	}
	wantAll := modelinfo.TokenCounts{Input: 14, Output: 3000, CacheRead: 12000, CacheWrite: 140000}
	if sum != wantAll {
		t.Errorf("parent + subagents = %+v, want %+v — the split must conserve every class", sum, wantAll)
	}
	if total, want := turnTotalCost(got), wantParent+wantSub; math.Abs(total-want) > 1e-9 {
		t.Errorf("turn total = %.9f, want %.9f", total, want)
	}
}

// TestOnResult_NoSubagentsLeavesTheRowWhole pins the common case: a turn that
// ran no subagents must be byte-for-byte what it was before the split existed,
// with no empty subagent row and the full cost on the parent.
func TestOnResult_NoSubagentsLeavesTheRowWhole(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	b.noteAssistantUsage(fullMsg("claude-opus-5", "msg_top", "", 10, 1000, 5000, 0, 40000))

	got := onResultWith(b, map[string]ModelUsage{
		"claude-opus-5": {InputTokens: 10, OutputTokens: 1000, CacheReadInputTokens: 5000, CacheCreationInputTokens: 40000, CostUSD: 0.4},
	})
	if got == nil || got.Usage == nil || got.Usage.CalculatedCostUSD == nil {
		t.Fatal("no calculated cost")
	}
	if len(got.Usage.Subagents) != 0 {
		t.Errorf("Subagents = %+v, want none — a turn with no subagents must write one row", got.Usage.Subagents)
	}
	want := 10*5.0/1e6 + 1000*25.0/1e6 + 5000*0.5/1e6 + 40000*10.0/1e6
	if math.Abs(*got.Usage.CalculatedCostUSD-want) > 1e-9 {
		t.Errorf("cost = %.9f, want %.9f", *got.Usage.CalculatedCostUSD, want)
	}
}

// TestOnResult_SubagentBucketOverAuthoritativeClampsAtZero pins the failure
// mode rather than leaving it to arithmetic.
//
// If the accumulator ever counts more than ModelUsage reports, the parent share
// goes negative — and a negative token count prices as a CREDIT, quietly
// reducing the bill. Stopping at zero makes the turn price ABOVE the
// authoritative total instead, which the divergence check is there to catch.
func TestOnResult_SubagentBucketOverAuthoritativeClampsAtZero(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	// The subagent alone exceeds what ModelUsage reports for the model.
	b.noteAssistantUsage(fullMsg("claude-opus-5", "msg_sub", "agent-x", 0, 5000, 0, 0, 0))

	got := onResultWith(b, map[string]ModelUsage{
		"claude-opus-5": {OutputTokens: 1000, CostUSD: 0.1},
	})
	if got == nil || got.Usage == nil || got.Usage.Turn == nil {
		t.Fatal("no turn counts")
	}
	if got.Usage.Turn.Output != 0 {
		t.Errorf("parent output = %d, want 0 — a negative share must pin at zero, never price as a credit",
			got.Usage.Turn.Output)
	}
	if *got.Usage.CalculatedCostUSD < 0 {
		t.Errorf("parent cost = %.9f, want >= 0", *got.Usage.CalculatedCostUSD)
	}
}

// TestOnResult_ParentKeepsItsOwnTTLWhenTheSubagentSharesItsModel exists because
// the first version of the test above could not see this at all: its parent and
// subagent ran DIFFERENT models, so the per-model combined split and the
// main-thread-only split were the same map either way, and swapping one for the
// other reddened nothing.
//
// The distinguishing case is one model with both TTLs in play. The parent here
// writes 5m and the subagent 1h — deliberately the reverse of the usual
// arrangement, because the code refuses to infer TTL from which bucket the
// tokens came from (that inference is what caused #1866) and a test that only
// ever ran it the usual way round would let the inference back in.
//
// Priced from the COMBINED split, the parent's 60,000 5m writes come back as
// 40,000 at 1h plus 20,000 Unknown — also 1h — for $0.600 instead of $0.375, a
// 60% overcharge on exactly the tokens #1866 was about.
func TestOnResult_ParentKeepsItsOwnTTLWhenTheSubagentSharesItsModel(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	b.noteAssistantUsage(fullMsg("claude-opus-5", "msg_top", "", 0, 0, 0, 60000, 0))
	b.noteAssistantUsage(fullMsg("claude-opus-5", "msg_sub", "agent-x", 0, 0, 0, 0, 40000))

	got := onResultWith(b, map[string]ModelUsage{
		"claude-opus-5": {CacheCreationInputTokens: 100000, CostUSD: 0.7},
	})
	if got == nil || got.Usage == nil || got.Usage.CalculatedCostUSD == nil {
		t.Fatal("no calculated cost")
	}
	wantParent := 60000 * 6.25 / 1e6 // 5m
	if math.Abs(*got.Usage.CalculatedCostUSD-wantParent) > 1e-9 {
		t.Errorf("parent cost = %.9f, want %.9f — the parent must be priced at ITS OWN observed TTL, not the turn's combined one",
			*got.Usage.CalculatedCostUSD, wantParent)
	}
	if len(got.Usage.Subagents) != 1 {
		t.Fatalf("Subagents = %+v, want exactly one", got.Usage.Subagents)
	}
	wantSub := 40000 * 10.0 / 1e6 // 1h
	if math.Abs(got.Usage.Subagents[0].CostUSD-wantSub) > 1e-9 {
		t.Errorf("subagent cost = %.9f, want %.9f", got.Usage.Subagents[0].CostUSD, wantSub)
	}
	// And the writes still reconstruct exactly.
	if sum := got.Usage.Turn.CacheWrite + got.Usage.Subagents[0].Counts.CacheWrite; sum != 100000 {
		t.Errorf("cache writes parent+sub = %d, want 100000", sum)
	}
}

// onResultInTurn runs one turn end-to-end with a named turn id, so a test can
// span more than one turn and see where spend lands.
func onResultInTurn(b *Backend, turnID string, before func(), mu map[string]ModelUsage) *delegator.TurnResult {
	var got *delegator.TurnResult
	applyHandler(b, &testHandler{
		TurnID:         turnID,
		OnTurnComplete: func(r *delegator.TurnResult) { got = r },
	})
	b.mu.Lock()
	b.lastModel = "claude-opus-5"
	b.mu.Unlock()
	if before != nil {
		before()
	}
	b.OnResult(&ResultMessage{Subtype: "success", Result: "ok", ModelUsage: mu})
	return got
}

// TestOnResult_StragglerBooksToTheTurnThatSpawnedIt is the second half of #1880
// phase C, and the case the ticket was opened for.
//
// A background subagent keeps working after the turn that spawned it has
// finished. CC bills continuously, so its remaining tokens arrive during some
// LATER turn — often a short one. Measured: a 3.5-minute turn recorded as
// costing $11.71 because it had inherited 34 minutes of a subagent's work
// (api_calls 47495), and again at $11.97 and $13.60. An expensive turn and a
// cheap turn that closed at the wrong moment were indistinguishable.
//
// Turn 2 here does no work of its own beyond the straggler's arrival. Its own
// row must be free of that spend, and the straggler's row must name TURN 1.
func TestOnResult_StragglerBooksToTheTurnThatSpawnedIt(t *testing.T) {
	t.Parallel()
	b := &Backend{}

	// Turn 1 spawns agent-x, which reports 100 output tokens before the turn ends.
	turn1 := onResultInTurn(b, "sess@1", func() {
		b.noteAssistantUsage(fullMsg("claude-sonnet-5", "msg_s1", "agent-x", 0, 100, 0, 0, 0))
	}, map[string]ModelUsage{
		"claude-sonnet-5": {OutputTokens: 100, CostUSD: 0.01},
	})
	if turn1 == nil || len(turn1.Usage.Subagents) != 1 {
		t.Fatalf("turn 1 subagents = %+v, want one", turn1.Usage.Subagents)
	}
	if got := turn1.Usage.Subagents[0].TurnID; got != "sess@1" {
		t.Errorf("turn 1 subagent TurnID = %q, want sess@1", got)
	}

	// Turn 2: agent-x is still running and its remaining 900 output tokens land
	// here. The parent did nothing else.
	turn2 := onResultInTurn(b, "sess@2", func() {
		b.noteAssistantUsage(fullMsg("claude-sonnet-5", "msg_s2", "agent-x", 0, 900, 0, 0, 0))
	}, map[string]ModelUsage{
		"claude-sonnet-5": {OutputTokens: 1000, CostUSD: 0.1},
	})
	if turn2 == nil || turn2.Usage == nil || turn2.Usage.CalculatedCostUSD == nil {
		t.Fatal("turn 2 produced no cost")
	}

	// The straggler's row names the turn that STARTED the work, not the one it
	// arrived during. This is the assertion the whole ticket is about.
	if len(turn2.Usage.Subagents) != 1 {
		t.Fatalf("turn 2 subagents = %+v, want one", turn2.Usage.Subagents)
	}
	sc := turn2.Usage.Subagents[0]
	if sc.TurnID != "sess@1" {
		t.Errorf("straggler TurnID = %q, want sess@1 — late spend books to the turn that spawned the agent, not the one that closed",
			sc.TurnID)
	}
	if sc.Counts.Output != 900 {
		t.Errorf("straggler output = %d, want 900 (the delta, not the running total)", sc.Counts.Output)
	}

	// And turn 2's own row carries none of it.
	if *turn2.Usage.CalculatedCostUSD != 0 || turn2.Usage.Turn.Output != 0 {
		t.Errorf("turn 2 parent = $%.9f / %d output, want 0/0 — a turn that did no work of its own must not inherit a straggler's",
			*turn2.Usage.CalculatedCostUSD, turn2.Usage.Turn.Output)
	}
}

// TestOnResult_SecondAgentInALaterTurnGetsThatTurn is the disambiguating
// control for the test above. Without it, "always return the first turn ever
// seen" would pass — the mapping has to be PER AGENT, not a single remembered
// turn.
func TestOnResult_SecondAgentInALaterTurnGetsThatTurn(t *testing.T) {
	t.Parallel()
	b := &Backend{}

	onResultInTurn(b, "sess@1", func() {
		b.noteAssistantUsage(fullMsg("claude-sonnet-5", "msg_a", "agent-old", 0, 100, 0, 0, 0))
	}, map[string]ModelUsage{"claude-sonnet-5": {OutputTokens: 100, CostUSD: 0.01}})

	// Turn 2 spawns a DIFFERENT agent, which must book to turn 2.
	turn2 := onResultInTurn(b, "sess@2", func() {
		b.noteAssistantUsage(fullMsg("claude-sonnet-5", "msg_b", "agent-new", 0, 50, 0, 0, 0))
	}, map[string]ModelUsage{"claude-sonnet-5": {OutputTokens: 150, CostUSD: 0.02}})

	if len(turn2.Usage.Subagents) != 1 {
		t.Fatalf("turn 2 subagents = %+v, want one (agent-old did no work this turn)", turn2.Usage.Subagents)
	}
	sc := turn2.Usage.Subagents[0]
	if sc.AgentID != "agent-new" || sc.TurnID != "sess@2" {
		t.Errorf("agent=%q turn=%q, want agent-new/sess@2 — a new agent belongs to the turn that spawned IT",
			sc.AgentID, sc.TurnID)
	}
}
