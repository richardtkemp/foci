package ccstream

import (
	"math"
	"testing"

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
