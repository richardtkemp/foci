package ccstream

import (
	"testing"

	"foci/internal/delegator"
)

// usage builds a TokenUsage with an explicit TTL split.
func usage(in, out, cr, cw, e5m, e1h int) TokenUsage {
	u := TokenUsage{InputTokens: in, OutputTokens: out, CacheReadInputTokens: cr, CacheCreationInputTokens: cw}
	if e5m > 0 || e1h > 0 {
		u.CacheCreation = &CacheCreationSplit{Ephemeral5m: e5m, Ephemeral1h: e1h}
	}
	return u
}

// TestUsageAccumulator_ReconcilesToModelUsage_Foreground is THE PHASE GATE.
//
// It replays the foreground probe (CC 2.1.261) exactly. The parent stream
// carried only 2 of the subagent's 3 messages — a pure-text message is
// suppressed from the parent stream ALONG WITH ITS USAGE — so the stream alone
// sums to 36,349 against a ModelUsage of 36,586. The transcript supplies the
// missing 237. Until the accumulator hits 36,586 it is not a complete source
// and cannot be priced from.
func TestUsageAccumulator_ReconcilesToModelUsage_Foreground(t *testing.T) {
	t.Parallel()
	const modelUsageCacheCreation = 36586 // measured: result.modelUsage, the authoritative total
	b := &Backend{}

	// Parent stream, top-level. Blocks repeated as CC actually emits them.
	for i := 0; i < 3; i++ {
		b.noteAssistantUsage(msgModel("claude-sonnet-4-5", "msg_top1", "", 21935, 0, 21935))
	}
	for i := 0; i < 2; i++ {
		b.noteAssistantUsage(msgModel("claude-sonnet-4-5", "msg_top2", "", 551, 0, 551))
	}
	// Parent stream, the 2 subagent messages that DID arrive.
	b.noteAssistantUsage(msgModel("claude-sonnet-4-5", "msg_sub1", "toolu_x", 12978, 12978, 0))
	b.noteAssistantUsage(msgModel("claude-sonnet-4-5", "msg_sub2", "toolu_x", 885, 885, 0))

	streamOnly := b.turnUsageAcc.writeSplit(false).addSplit(b.turnUsageAcc.writeSplit(true)).total()
	if streamOnly != 36349 {
		t.Fatalf("stream-only total = %d, want 36349 (the measured shortfall state)", streamOnly)
	}
	if streamOnly == modelUsageCacheCreation {
		t.Fatal("stream alone already reconciles — the fixture no longer reproduces the gap it tests")
	}

	// The transcript supplies the suppressed message.
	b.noteSubagentTranscriptUsage("toolu_x", "claude-sonnet-4-5", "msg_sub3", usage(2, 138, 0, 237, 237, 0))

	got := b.turnUsageAcc.writeSplit(false).addSplit(b.turnUsageAcc.writeSplit(true)).total()
	if got != modelUsageCacheCreation {
		t.Errorf("accumulator total = %d, want %d (ModelUsage) — source is incomplete, cannot be priced from",
			got, modelUsageCacheCreation)
	}
	if sub := b.turnUsageAcc.writeSplit(true).total(); sub != 14100 {
		t.Errorf("subagent total = %d, want 14100 (its transcript's own figure)", sub)
	}
}

// TestUsageAccumulator_ForegroundArrivesTwiceAndIsCountedOnce: the tail runs
// only for FOREGROUND subagents, and the parent stream still carries some of
// that same subagent's messages — the probe saw 2 of its 3 in the stream and
// all 3 in the transcript. So those two arrive by both routes, and the dedupe
// is what stops them doubling. (Background subagents get no tail, so they
// arrive by the stream alone.)
func TestUsageAccumulator_ForegroundArrivesTwiceAndIsCountedOnce(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	b.noteAssistantUsage(msgModel("claude-opus-5", "msg_A", "toolu_x", 11959, 11959, 0))
	b.noteSubagentTranscriptUsage("toolu_x", "claude-opus-5", "msg_A", usage(2, 5, 0, 11959, 11959, 0))

	if got := b.turnUsageAcc.writeSplit(true).total(); got != 11959 {
		t.Errorf("total = %d, want 11959 — counted %.1fx across the two routes", got, float64(got)/11959)
	}
}

// TestUsageAccumulator_BucketsByModel: a turn routinely spans several models
// (73% of subagents run one different from their parent). Pricing needs each
// model's tokens separately, because each has its own rates.
func TestUsageAccumulator_BucketsByModel(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	b.noteAssistantUsage(msgModel("claude-opus-5", "msg_p", "", 8320, 0, 8320))
	b.noteAssistantUsage(msgModel("claude-sonnet-5", "msg_s", "toolu_x", 19357, 19357, 0))
	b.noteAssistantUsage(msgModel("claude-haiku-4-5", "msg_h", "toolu_y", 51591, 51591, 0))

	models := b.turnUsageAcc.models()
	if len(models) != 3 {
		t.Fatalf("models = %v, want 3 distinct", models)
	}
	// Read through a nil-safe helper deliberately: a test that PANICS on a
	// missing bucket aborts the whole binary, so every other test in the
	// package reports nothing. That turns one clear failure into six silent
	// ones, which is worse than the bug being caught.
	// The sub bucket is keyed by agent AND model (#1880 phase C), so sum every
	// agent that used the model rather than indexing one key.
	subWrite := func(model string, pick func(cacheWriteSplit) int) int {
		found := false
		total := 0
		for k, tu := range b.turnUsageAcc.sub {
			if k.Model == model {
				found = true
				total += pick(tu.Write)
			}
		}
		if !found {
			return -1
		}
		return total
	}
	write1h := func(m map[string]*turnUsage, model string) int {
		if tu := m[model]; tu != nil {
			return tu.Write.Ephemeral1h
		}
		return -1
	}
	if got := subWrite("claude-sonnet-5", func(w cacheWriteSplit) int { return w.Ephemeral5m }); got != 19357 {
		t.Errorf("sonnet-5 subagent 5m = %d, want 19357 (-1 = no such bucket)", got)
	}
	if got := subWrite("claude-haiku-4-5", func(w cacheWriteSplit) int { return w.Ephemeral5m }); got != 51591 {
		t.Errorf("haiku subagent 5m = %d, want 51591 (-1 = no such bucket)", got)
	}
	if got := write1h(b.turnUsageAcc.top, "claude-opus-5"); got != 8320 {
		t.Errorf("opus parent 1h = %d, want 8320 (-1 = no such bucket)", got)
	}
	// The parent model's own bucket must NOT have absorbed the subagents'
	// tokens — that conflation is exactly what drops their spend (#1872).
	if _, ok := b.turnUsageAcc.top["claude-sonnet-5"]; ok {
		t.Error("sonnet-5 leaked into the top-level bucket")
	}
}

// TestUsageAccumulator_AllFourClasses: phase 1 tracked cache writes only.
// Pricing needs every class, or the accumulator cannot replace ModelUsage.
func TestUsageAccumulator_AllFourClasses(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	b.noteAssistantUsage(msgModel("claude-opus-5", "msg_A", "", 690, 0, 690))
	b.noteSubagentTranscriptUsage("toolu_x", "claude-opus-5", "msg_B", usage(30, 11693, 1038737, 1022, 1022, 0))

	sub := b.turnUsageAcc.sub[subKey{Agent: "toolu_x", Model: "claude-opus-5"}]
	if sub == nil {
		t.Fatal("no subagent bucket for claude-opus-5 — transcript usage was not recorded at all")
	}
	if sub.Input != 30 || sub.Output != 11693 || sub.CacheRead != 1038737 {
		t.Errorf("in/out/cacheRead = %d/%d/%d, want 30/11693/1038737", sub.Input, sub.Output, sub.CacheRead)
	}
}

// TestDeliverLine_RecordsUsageWithNoTextSink: the two sinks fail independently.
// deliverLine used to return early when deliver was nil, which would have
// discarded every token a subagent spent whenever the consumer had no text
// sink. Losing display is not the same as losing money.
func TestDeliverLine_RecordsUsageWithNoTextSink(t *testing.T) {
	t.Parallel()
	var gotModel, gotID string
	var gotUsage TokenUsage
	mgr := newSubagentTailManager(nil, func(_, model, id string, u TokenUsage) {
		gotModel, gotID, gotUsage = model, id, u
	}, nil)

	line := []byte(`{"type":"assistant","isSidechain":true,"message":{"id":"msg_Z","model":"claude-opus-5",` +
		`"usage":{"input_tokens":2,"output_tokens":138,"cache_creation_input_tokens":11534,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":11534,"ephemeral_1h_input_tokens":0}},` +
		`"content":[{"type":"text","text":"hi"}]}}`)
	mgr.deliverLine("toolu_x", line, false)

	if gotID != "msg_Z" || gotModel != "claude-opus-5" {
		t.Errorf("id/model = %q/%q, want msg_Z/claude-opus-5", gotID, gotModel)
	}
	if gotUsage.CacheCreation == nil || gotUsage.CacheCreation.Ephemeral5m != 11534 {
		t.Errorf("5m usage not recorded: %+v", gotUsage)
	}
}

// TestDeliverLine_IgnoresNonAssistantRecords guards the obvious over-count: a
// transcript holds the input prompt, tool_use and tool_result records too.
func TestDeliverLine_IgnoresNonAssistantRecords(t *testing.T) {
	t.Parallel()
	calls := 0
	mgr := newSubagentTailManager(nil, func(string, string, string, TokenUsage) { calls++ }, nil)
	mgr.deliverLine("toolu_x", []byte(`{"type":"user","message":{"id":"msg_U","content":[]}}`), false)
	mgr.deliverLine("toolu_x", []byte(`not json`), false)
	if calls != 0 {
		t.Errorf("noteUsage called %d times for non-assistant records, want 0", calls)
	}
}

// TestUsageAccumulator_AttributesSpendPerSubagent is the #1880 phase C
// capability: spend traced to the subagent that INCURRED it, not merely to the
// fact that some subagent did.
//
// The sub bucket used to be keyed by model alone, so two subagents on the same
// model were indistinguishable — and a background subagent outliving its parent
// was attributed to whichever turn happened to close while it ran. The agent
// key is the Agent tool_use id, which also names the transcript file, so a row
// written from it can be traced back to the work.
func TestUsageAccumulator_AttributesSpendPerSubagent(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	// Parent.
	b.noteAssistantUsage(msgModel("claude-opus-5", "msg_top", "", 8320, 0, 8320))
	// Two subagents on the SAME model — indistinguishable under the old key.
	b.noteAssistantUsage(msgModel("claude-sonnet-5", "msg_a1", "toolu_A", 1000, 1000, 0))
	b.noteAssistantUsage(msgModel("claude-sonnet-5", "msg_b1", "toolu_B", 2000, 2000, 0))
	// One of them also uses a second model (it spawned its own).
	b.noteAssistantUsage(msgModel("claude-haiku-4-5", "msg_a2", "toolu_A", 500, 500, 0))

	got := b.turnUsageAcc.subagentUsage()
	if len(got) != 2 {
		t.Fatalf("subagents = %d, want 2 (toolu_A, toolu_B); got %+v", len(got), got)
	}
	if w := got["toolu_A"].Write.Ephemeral5m; w != 1500 {
		t.Errorf("toolu_A 5m = %d, want 1500 — both its models must sum under one agent", w)
	}
	if w := got["toolu_B"].Write.Ephemeral5m; w != 2000 {
		t.Errorf("toolu_B 5m = %d, want 2000", w)
	}
	// The parent's own spend must not appear as a subagent's.
	if _, leaked := got[""]; leaked {
		t.Error("top-level spend leaked into the subagent attribution")
	}
}

// TestUsageAccumulator_SubagentAttributionIsTurnScoped: like every other
// accessor, per-agent usage is measured from the turn baseline. Returning
// running totals would report a subagent's whole life against one turn — the
// same per-SESSION-beside-per-TURN error as #1848.
func TestUsageAccumulator_SubagentAttributionIsTurnScoped(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	b.noteAssistantUsage(msgModel("claude-sonnet-5", "msg_old", "toolu_A", 5000, 5000, 0))
	b.turnUsageAcc.markResult()
	b.beginTurnLocked(&delegator.TurnEvents{})

	if got := b.turnUsageAcc.subagentUsage()["toolu_A"].Write.Ephemeral5m; got != 0 {
		t.Errorf("carried %d tokens of a previous turn into this one", got)
	}
	b.noteAssistantUsage(msgModel("claude-sonnet-5", "msg_new", "toolu_A", 700, 700, 0))
	if got := b.turnUsageAcc.subagentUsage()["toolu_A"].Write.Ephemeral5m; got != 700 {
		t.Errorf("this turn's figure = %d, want 700", got)
	}
}
