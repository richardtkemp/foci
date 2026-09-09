package ccstream

import (
	"strings"
	"testing"

	"foci/internal/delegator"
	"foci/internal/modelinfo"
)

// msgWith builds one assistant stream line. CC repeats the SAME message id once
// per content block with identical usage, so the tests below construct repeats
// explicitly rather than assuming one line per API call.
func msgWith(id string, parent string, cacheWrite int, e5m, e1h int) *AssistantMessage {
	m := &AssistantMessage{}
	m.Message.ID = id
	m.Message.Usage = TokenUsage{CacheCreationInputTokens: cacheWrite}
	if e5m > 0 || e1h > 0 {
		m.Message.Usage.CacheCreation = &CacheCreationSplit{Ephemeral5m: e5m, Ephemeral1h: e1h}
	}
	if parent != "" {
		m.ParentToolUseID = &parent
	}
	return m
}

// TestNoteCacheWriteSplit_DedupesContentBlockRepeats is the load-bearing arm.
//
// CC emits one assistant line PER CONTENT BLOCK, each carrying the whole
// message's usage (probe-verified: a thinking+text+tool_use message arrived
// three times with identical counts). Accumulating them all inflates by the
// block count — 3x here — which is not a rounding error, it is a wrong bill.
func TestNoteCacheWriteSplit_DedupesContentBlockRepeats(t *testing.T) {
	t.Parallel()
	b := &Backend{}

	// One API call, three content blocks, as CC actually streams it.
	for i := 0; i < 3; i++ {
		b.noteCacheWriteSplit(msgWith("msg_A", "", 8320, 0, 8320))
	}

	if got := b.turnWriteTop.Ephemeral1h; got != 8320 {
		t.Errorf("Ephemeral1h = %d, want 8320 (counted %.1fx — dedupe by message id failed)",
			got, float64(got)/8320)
	}
	if got := b.turnWriteTop.total(); got != 8320 {
		t.Errorf("total = %d, want 8320", got)
	}
}

// TestNoteCacheWriteSplit_PartitionsSubagentFromTopLevel replays the exact
// message sequence captured from the background probe (CC 2.1.261), including
// its content-block repeats. Its totals are known independently: the deduped
// sum reconciled EXACTLY to the result's ModelUsage (23,727) and the subagent
// subset EXACTLY to that subagent's own transcript (12,939).
func TestNoteCacheWriteSplit_PartitionsSubagentFromTopLevel(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	const sub = "toolu_01F4WietrMRVdUPv8Lie64qA"

	// Top-level: 1h exclusively. Blocks repeated as observed.
	for i := 0; i < 3; i++ {
		b.noteCacheWriteSplit(msgWith("msg_top1", "", 8320, 0, 8320))
	}
	for i := 0; i < 3; i++ {
		b.noteCacheWriteSplit(msgWith("msg_top2", "", 1044, 0, 1044))
	}
	for i := 0; i < 2; i++ {
		b.noteCacheWriteSplit(msgWith("msg_top3", "", 781, 0, 781))
	}
	for i := 0; i < 2; i++ {
		b.noteCacheWriteSplit(msgWith("msg_top4", "", 643, 0, 643))
	}
	// Subagent: 5m exclusively.
	for i := 0; i < 3; i++ {
		b.noteCacheWriteSplit(msgWith("msg_sub1", sub, 11959, 11959, 0))
	}
	for i := 0; i < 3; i++ {
		b.noteCacheWriteSplit(msgWith("msg_sub2", sub, 786, 786, 0))
	}
	for i := 0; i < 2; i++ {
		b.noteCacheWriteSplit(msgWith("msg_sub3", sub, 194, 194, 0))
	}

	if got := b.turnWriteTop.total(); got != 10788 {
		t.Errorf("top-level total = %d, want 10788", got)
	}
	if b.turnWriteTop.Ephemeral5m != 0 {
		t.Errorf("top-level 5m = %d, want 0 — the main thread caches at 1h", b.turnWriteTop.Ephemeral5m)
	}
	if got := b.turnWriteSub.total(); got != 12939 {
		t.Errorf("subagent total = %d, want 12939 (its transcript's own figure)", got)
	}
	if b.turnWriteSub.Ephemeral1h != 0 {
		t.Errorf("subagent 1h = %d, want 0 — subagents cache at 5m", b.turnWriteSub.Ephemeral1h)
	}
	// The reconciliation that matters: everything must add back to ModelUsage.
	if got := b.turnWriteTop.addSplit(b.turnWriteSub).total(); got != 23727 {
		t.Errorf("combined total = %d, want 23727 (the result's ModelUsage cacheCreationInputTokens)", got)
	}
}

// TestNoteCacheWriteSplit_UnknownWhenCCReportsNoBreakdown: absent a breakdown
// the TTL is UNOBSERVED, which is a different fact from "it was 5m". Recording
// it as either rate is the assumption that caused #1866 in the first place.
func TestNoteCacheWriteSplit_UnknownWhenCCReportsNoBreakdown(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	b.noteCacheWriteSplit(msgWith("msg_A", "", 5000, 0, 0))

	w := b.turnWriteTop
	if w.Unknown != 5000 {
		t.Errorf("Unknown = %d, want 5000", w.Unknown)
	}
	if w.Ephemeral5m != 0 || w.Ephemeral1h != 0 {
		t.Errorf("5m=%d 1h=%d, want both 0 — an absent breakdown must not be guessed",
			w.Ephemeral5m, w.Ephemeral1h)
	}
	if w.total() != 5000 {
		t.Errorf("total = %d, want 5000 — unknown-TTL tokens must still reconcile", w.total())
	}
}

// TestNoteCacheWriteSplit_ShortfallLandsInUnknown: if CC's parts do not sum to
// the merged figure, the difference must survive somewhere or the accumulator
// silently disagrees with the ModelUsage total it is meant to explain.
func TestNoteCacheWriteSplit_ShortfallLandsInUnknown(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	b.noteCacheWriteSplit(msgWith("msg_A", "", 1000, 400, 500))

	if got := b.turnWriteTop.Unknown; got != 100 {
		t.Errorf("Unknown = %d, want 100 (1000 merged - 400 5m - 500 1h)", got)
	}
	if got := b.turnWriteTop.total(); got != 1000 {
		t.Errorf("total = %d, want 1000 — must reconcile to the merged figure", got)
	}
}

// TestBeginTurn_ResetsCacheWriteSplit puts the new fields under the SAME
// boundary as the rest of the cost group. #1848 shipped because half that group
// was reset at neither call site, and the fix was to give the group one name;
// a field added outside it re-creates the bug.
func TestBeginTurn_ResetsCacheWriteSplit(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	b.noteCacheWriteSplit(msgWith("msg_A", "", 8320, 0, 8320))
	b.noteCacheWriteSplit(msgWith("msg_B", "tool_1", 11959, 11959, 0))

	b.beginTurnLocked(&delegator.TurnEvents{})

	if b.turnWriteTop != (cacheWriteSplit{}) {
		t.Errorf("turnWriteTop = %+v after beginTurnLocked, want zero", b.turnWriteTop)
	}
	if b.turnWriteSub != (cacheWriteSplit{}) {
		t.Errorf("turnWriteSub = %+v after beginTurnLocked, want zero", b.turnWriteSub)
	}
	// The dedupe set must reset too: a stale id would silently DROP a genuine
	// message in the next turn, which reads as an under-count, not a crash.
	if b.turnWriteSeen != nil {
		t.Errorf("turnWriteSeen = %v after beginTurnLocked, want nil", b.turnWriteSeen)
	}
	b.noteCacheWriteSplit(msgWith("msg_A", "", 500, 0, 500))
	if got := b.turnWriteTop.Ephemeral1h; got != 500 {
		t.Errorf("re-used message id counted %d after a turn boundary, want 500", got)
	}
}

// TestCostBreakdown_NamesTheCacheWriteTTLMix: the whole point of phase 1 is that
// the next divergence warning says WHY. A turn billed at the 1h rate whose
// writes were 5m must be readable as such from the log line alone.
func TestCostBreakdown_NamesTheCacheWriteTTLMix(t *testing.T) {
	t.Parallel()
	bd := costBreakdown{
		model:    "claude/claude-fable-5-1",
		cycles:   1,
		counts:   modelinfo.TokenCounts{Input: 38, Output: 2098, CacheRead: 593656, CacheWrite: 14609},
		writeTop: cacheWriteSplit{Ephemeral1h: 3075},
		writeSub: cacheWriteSplit{Ephemeral5m: 11534},
	}
	got := bd.String()

	for _, want := range []string{"cache_write_ttl", "5m=11534", "1h=3075", "subagent 11534 of 14609"} {
		if !strings.Contains(got, want) {
			t.Errorf("breakdown missing %q — the warning cannot name its own cause\ngot: %s", want, got)
		}
	}
}

// TestCostBreakdown_SilentWhenNoCacheWrites: an all-1h main-thread turn is the
// normal shape and adds nothing by saying so. A suffix on every line is noise,
// and noise is what makes a real warning get skipped.
func TestCostBreakdown_SilentWhenNoCacheWrites(t *testing.T) {
	t.Parallel()
	bd := costBreakdown{model: "claude/claude-fable-5-1", cycles: 1}
	if got := bd.String(); strings.Contains(got, "cache_write_ttl") {
		t.Errorf("breakdown named a TTL mix with no cache writes at all\ngot: %s", got)
	}
}
