package ccstream

import "testing"

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
	b.noteSubagentTranscriptUsage("claude-sonnet-4-5", "msg_sub3", usage(2, 138, 0, 237, 237, 0))

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
	b.noteSubagentTranscriptUsage("claude-opus-5", "msg_A", usage(2, 5, 0, 11959, 11959, 0))

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
	write5m := func(m map[string]*turnUsage, model string) int {
		if tu := m[model]; tu != nil {
			return tu.Write.Ephemeral5m
		}
		return -1
	}
	write1h := func(m map[string]*turnUsage, model string) int {
		if tu := m[model]; tu != nil {
			return tu.Write.Ephemeral1h
		}
		return -1
	}
	if got := write5m(b.turnUsageAcc.sub, "claude-sonnet-5"); got != 19357 {
		t.Errorf("sonnet-5 subagent 5m = %d, want 19357 (-1 = no such bucket)", got)
	}
	if got := write5m(b.turnUsageAcc.sub, "claude-haiku-4-5"); got != 51591 {
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
	b.noteSubagentTranscriptUsage("claude-opus-5", "msg_B", usage(30, 11693, 1038737, 1022, 1022, 0))

	sub := b.turnUsageAcc.sub["claude-opus-5"]
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
	mgr := newSubagentTailManager(nil, func(model, id string, u TokenUsage) {
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
	mgr := newSubagentTailManager(nil, func(string, string, TokenUsage) { calls++ }, nil)
	mgr.deliverLine("toolu_x", []byte(`{"type":"user","message":{"id":"msg_U","content":[]}}`), false)
	mgr.deliverLine("toolu_x", []byte(`not json`), false)
	if calls != 0 {
		t.Errorf("noteUsage called %d times for non-assistant records, want 0", calls)
	}
}
