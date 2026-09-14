package ccstream

import (
	"testing"
	"time"
)

// TestSubagentDelta_RetroactiveUsageStaysOutOfThisTurn replays the live
// overcharge of 2026-09-13 15:41:50 (#1909).
//
// A background subagent spent 2,445,048 cache-read tokens during an EARLIER
// turn. Its transcript lines reached foci in a catch-up burst inside a later
// turn whose own authoritative ModelUsage was 2,155,142. Because the
// accumulator bucketed by ARRIVAL and ModelUsage buckets by BILLING, the
// parent's share was computed as 2,155,142 minus 2,445,048 — negative, so
// clamped at zero — and the turn was priced at parent($0) + subagent, i.e.
// ABOVE the authoritative total it was derived from. Live cost: $2.7853
// charged against a true $1.8745, 48.6% over.
//
// The fix reads each transcript line's own timestamp. A message billed before
// the window opened was already inside an earlier turn's ModelUsage and has
// already been paid for, so it must not be subtracted from this turn's.
func TestSubagentDelta_RetroactiveUsageStaysOutOfThisTurn(t *testing.T) {
	t.Parallel()

	var (
		duringPrevTurn = time.Date(2026, 9, 13, 14, 30, 0, 0, time.UTC)
		prevResult     = time.Date(2026, 9, 13, 14, 41, 35, 0, time.UTC)
		duringThisTurn = time.Date(2026, 9, 13, 14, 41, 40, 0, time.UTC)
	)

	a := &usageAccumulator{}
	a.markResult(prevResult) // closes the previous window, at that instant
	a.beginTurn("turn-B")    // windowStart = prevResult

	// The catch-up burst: billed 11 minutes before this window opened.
	a.note("claude-opus-5", "agent-1", true, "msg_late", duringPrevTurn,
		usage(0, 0, 2445048, 0, 0, 0))
	// Work genuinely done inside this window.
	a.note("claude-opus-5", "agent-1", true, "msg_now", duringThisTurn,
		usage(0, 0, 120000, 0, 0, 0))

	d := a.subagentDelta()[subKey{Agent: "agent-1", Model: "claude-opus-5"}]
	if d.CacheRead != 120000 {
		t.Errorf("this turn's subagent cache-read = %d, want 120000 — the 2,445,048 "+
			"billed before the window opened was already inside the previous turn's "+
			"ModelUsage and must not be charged again (#1909)", d.CacheRead)
	}

	// The clamp is an EXACT detector: with both sides on one clock the
	// subagent share can never exceed the window's authoritative total, so
	// the parent never clamps and parent+sub reconstructs the total.
	const modelUsageCacheRead = 2155142 // the live figure for that turn
	if d.CacheRead > modelUsageCacheRead {
		t.Errorf("subagent share %d still exceeds ModelUsage %d — the parent would "+
			"clamp at zero and the turn price above its own authority",
			d.CacheRead, modelUsageCacheRead)
	}

	// Session totals are untouched: re-bucketing moves spend between windows,
	// it never creates or destroys any. If this drops, the fix is losing money
	// rather than re-filing it.
	if got := a.sub[subKey{Agent: "agent-1", Model: "claude-opus-5"}].CacheRead; got != 2565048 {
		t.Errorf("cumulative subagent cache-read = %d, want 2565048 — re-bucketing "+
			"must be conservative", got)
	}
}

// TestSubagentDelta_WindowStartIsTheLastResultNotTheTurnOpen pins the boundary.
//
// A turn is priced from the PREVIOUS RESULT, not from when it opened: messages
// arriving in the gap between the two are inside the window that gets priced
// (that gap is what lost 85-99% of cache-write tokens in #1880). The event-time
// boundary has to sit in exactly the same place, or spend that belongs to this
// turn by tokens belongs to no turn at all by time.
func TestSubagentDelta_WindowStartIsTheLastResultNotTheTurnOpen(t *testing.T) {
	t.Parallel()

	lastResult := time.Date(2026, 9, 13, 14, 41, 35, 0, time.UTC)
	inTheGap := lastResult.Add(2 * time.Second) // after the result, before the turn opened

	a := &usageAccumulator{}
	a.markResult(lastResult)
	a.beginTurn("turn-B")

	a.note("claude-opus-5", "agent-1", true, "msg_gap", inTheGap, usage(0, 0, 50000, 0, 0, 0))

	d := a.subagentDelta()[subKey{Agent: "agent-1", Model: "claude-opus-5"}]
	if d.CacheRead != 50000 {
		t.Errorf("gap usage = %d, want 50000 — a message billed after the last result "+
			"is inside this turn's priced window even though the turn opened later",
			d.CacheRead)
	}
}

// TestNote_UntimestampedUsageIsNeverTreatedAsRetroactive guards the degrade
// path. Parent-stream messages carry no timestamp and neither does a transcript
// line whose timestamp CC stops writing or changes format. All of those arrive
// with the zero time, which must mean "unknown — treat as now", never "billed
// at the epoch, therefore retroactive". Getting that backwards would silently
// zero every turn's subagent share instead of overcharging it.
func TestNote_UntimestampedUsageIsNeverTreatedAsRetroactive(t *testing.T) {
	t.Parallel()

	a := &usageAccumulator{}
	a.markResult(time.Date(2026, 9, 13, 14, 41, 35, 0, time.UTC))
	a.beginTurn("turn-B")

	a.note("claude-opus-5", "agent-1", true, "msg_nots", time.Time{}, usage(0, 0, 77000, 0, 0, 0))

	d := a.subagentDelta()[subKey{Agent: "agent-1", Model: "claude-opus-5"}]
	if d.CacheRead != 77000 {
		t.Errorf("untimestamped usage = %d, want 77000 — the zero time means unknown, "+
			"not the epoch", d.CacheRead)
	}
}

// TestDeliverLine_ParsesTranscriptTimestamp proves the timestamp reaches the
// accumulator from a REAL transcript line, not just from a hand-built call.
// Without this the three tests above would pass on a tail that never parses the
// field — the behaviour would be right in a test and absent in production.
func TestDeliverLine_ParsesTranscriptTimestamp(t *testing.T) {
	t.Parallel()

	var got time.Time
	mgr := newSubagentTailManager(nil, func(_, _, _ string, at time.Time, _ TokenUsage) {
		got = at
	}, nil)

	// Shape copied from a live transcript line (agent-ab47177003b2abea3.jsonl):
	// timestamp is TOP-LEVEL, RFC3339 with millis and a Z.
	line := `{"type":"assistant","isSidechain":true,"timestamp":"2026-09-13T14:40:13.820Z",` +
		`"message":{"id":"msg_1","model":"claude-opus-5","usage":{"cache_read_input_tokens":5}}}`
	mgr.deliverLine("agent-1", []byte(line), false)

	want := time.Date(2026, 9, 13, 14, 40, 13, 820000000, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parsed timestamp = %v, want %v — the tail is not reading the field, "+
			"so every message would look untimestamped and bucket by arrival (#1909)",
			got, want)
	}
}

// TestSubagentDelta_IsPerResultCycleNotPerTurn replays the OTHER half of
// #1909 — the one that actually fired, twice.
//
// The handler adds subagentDelta to the turn's rows once per RESULT CYCLE, and
// subtracts it from modelUsageDelta, which is itself per cycle. While
// subagentDelta reported cumulative-since-TURN-start, a two-cycle turn charged
// its subagent twice and pinned cycle 2's parent at zero.
//
// Numbers are the live turn of 2026-09-13 15:41:50 (clutch, ask_cycles=2).
func TestSubagentDelta_IsPerResultCycleNotPerTurn(t *testing.T) {
	t.Parallel()

	const (
		subCacheRead   = 1222524 // the subagent's spend, delivered during cycle 1
		cycle1ModelUse = 1843451 // ModelUsage delta for cycle 1
		cycle2ModelUse = 311691  // ModelUsage delta for cycle 2 (from the WARN)
		wantParent     = 620927  // the parent row api.db actually holds
	)
	k := subKey{Agent: "agent-1", Model: "claude-opus-5"}

	a := &usageAccumulator{}
	a.markResult(time.Date(2026, 9, 13, 14, 41, 35, 0, time.UTC))
	a.beginTurn("turn-live")

	a.note("claude-opus-5", "agent-1", true, "msg_sub",
		time.Date(2026, 9, 13, 14, 41, 40, 0, time.UTC), usage(0, 0, subCacheRead, 0, 0, 0))

	// ---- cycle 1 ----
	c1 := a.subagentDelta()[k]
	if c1.CacheRead != subCacheRead {
		t.Fatalf("cycle 1 subagent = %d, want %d", c1.CacheRead, subCacheRead)
	}
	parent1 := cycle1ModelUse - c1.CacheRead
	if parent1 != wantParent {
		t.Errorf("cycle 1 parent = %d, want %d (the row api.db holds)", parent1, wantParent)
	}
	a.markResult(time.Date(2026, 9, 13, 14, 41, 49, 0, time.UTC))

	// ---- cycle 2: the subagent contributed NOTHING new ----
	c2 := a.subagentDelta()[k]
	if c2.CacheRead != 0 {
		t.Errorf("cycle 2 subagent = %d, want 0 — nothing new arrived, so re-reporting "+
			"the turn's cumulative figure charges it a second time (#1909)", c2.CacheRead)
	}
	if c2.CacheRead > cycle2ModelUse {
		t.Errorf("cycle 2 subagent %d exceeds that cycle's authoritative total %d — "+
			"the parent clamps at zero and the turn prices above its own bill",
			c2.CacheRead, cycle2ModelUse)
	}

	// The turn's charge is the sum over cycles, exactly as the handler builds it.
	charged := parent1 + (cycle2ModelUse - c2.CacheRead) + c1.CacheRead + c2.CacheRead
	const bill = cycle1ModelUse + cycle2ModelUse // 2,155,142, the authoritative turn total
	if charged != bill {
		t.Errorf("turn charged %d against a bill of %d — parent+subagents must "+
			"reconstruct the total exactly once each", charged, bill)
	}
}

// TestSubagentUsage_StaysWholeTurnAcrossCycles guards the other direction.
//
// subagentDelta went per-cycle for pricing; the BREAKDOWN line must not follow
// it. costBreakdown.subagents describes the turn a reader asked about, so on a
// two-cycle turn it still has to report everything the subagent spent, not just
// whatever landed after the last result.
func TestSubagentUsage_StaysWholeTurnAcrossCycles(t *testing.T) {
	t.Parallel()

	a := &usageAccumulator{}
	a.markResult(time.Date(2026, 9, 13, 14, 41, 35, 0, time.UTC))
	a.beginTurn("turn-live")

	inTurn := time.Date(2026, 9, 13, 14, 41, 40, 0, time.UTC)
	a.note("claude-opus-5", "agent-1", true, "msg_c1", inTurn, usage(0, 0, 1000000, 0, 0, 0))
	a.markResult(time.Date(2026, 9, 13, 14, 41, 49, 0, time.UTC))
	a.note("claude-opus-5", "agent-1", true, "msg_c2",
		time.Date(2026, 9, 13, 14, 41, 50, 0, time.UTC), usage(0, 0, 222524, 0, 0, 0))

	if got := a.subagentDelta()[subKey{Agent: "agent-1", Model: "claude-opus-5"}].CacheRead; got != 222524 {
		t.Errorf("pricing delta = %d, want 222524 (cycle 2 only)", got)
	}
	if got := a.subagentUsage()["agent-1"].CacheRead; got != 1222524 {
		t.Errorf("breakdown = %d, want 1222524 (the whole turn) — the breakdown line "+
			"would under-report every multi-cycle turn", got)
	}
}

// TestBeginTurn_BaselinesDoNotShareMaps pins the aliasing hazard the late-usage
// path introduced. beginTurn used to assign usageTotals straight across, which
// copies the struct but SHARES its maps; note() now RAISES both baselines, so a
// shared map would apply one raise twice and silently discard live spend.
func TestBeginTurn_BaselinesDoNotShareMaps(t *testing.T) {
	t.Parallel()

	k := subKey{Agent: "agent-1", Model: "claude-opus-5"}
	a := &usageAccumulator{}
	a.markResult(time.Date(2026, 9, 13, 14, 41, 35, 0, time.UTC))
	a.beginTurn("turn-A")

	a.atLastResult.raiseSub(k, turnUsage{CacheRead: 5})
	if got := a.atTurnStart.sub[k].CacheRead; got != 0 {
		t.Errorf("raising atLastResult moved atTurnStart to %d — the baselines share "+
			"their maps, so every raise lands twice", got)
	}
}

// TestSubagentUsage_ExcludesRetroactiveUsageToo exists because a fail-arm found
// nothing to break.
//
// Late usage raises BOTH baselines, and removing the atTurnStart raise reddened
// no test at all — every retro assertion went through subagentDelta, which
// reads the other one. So the whole-turn breakdown could have gone on reporting
// spend that an earlier turn had already been charged for, with no test to say
// so. Money is priced from subagentDelta and was never at risk; the number a
// reader is shown was.
func TestSubagentUsage_ExcludesRetroactiveUsageToo(t *testing.T) {
	t.Parallel()

	a := &usageAccumulator{}
	a.markResult(time.Date(2026, 9, 13, 14, 41, 35, 0, time.UTC))
	a.beginTurn("turn-B")

	// Billed 11 minutes before this window opened: already inside an earlier
	// turn's ModelUsage, already paid for there.
	a.note("claude-opus-5", "agent-1", true, "msg_late",
		time.Date(2026, 9, 13, 14, 30, 0, 0, time.UTC), usage(0, 0, 2445048, 0, 0, 0))
	// Genuinely this turn's.
	a.note("claude-opus-5", "agent-1", true, "msg_now",
		time.Date(2026, 9, 13, 14, 41, 40, 0, time.UTC), usage(0, 0, 120000, 0, 0, 0))

	if got := a.subagentUsage()["agent-1"].CacheRead; got != 120000 {
		t.Errorf("breakdown subagent = %d, want 120000 — the breakdown reports spend "+
			"an earlier turn was already charged for", got)
	}
}
