package codex

import (
	"context"
	"testing"

	"foci/internal/delegator"
)

// Four consecutive thread/tokenUsage/updated notifications captured from a
// live codex 0.145.0 app-server (probe of #1855): one turn that ran three
// shell commands and then answered, so FOUR API cycles. codex sends the
// notification once per cycle, each carrying only that cycle's own figures in
// `last` — the turn total is nowhere on the wire except as the difference of
// two `total`s, which is why foci accumulates.
//
// The `total` values are carried too, as the arithmetic control: codex's own
// running sum for the final cycle is input=62617 cached=53376 output=151, and
// foci's accumulator must reproduce it exactly (input is stored
// cached-subtracted, so 62617 == turn.Input + turn.CacheRead).
const (
	usageCycle1 = `{"method":"thread/tokenUsage/updated","params":{"threadId":"th_1","turnId":"tu_1","tokenUsage":{"last":{"inputTokens":15509,"cachedInputTokens":11264,"cacheWriteInputTokens":0,"outputTokens":64,"reasoningOutputTokens":0,"totalTokens":15573},"total":{"inputTokens":15509,"cachedInputTokens":11264,"cacheWriteInputTokens":0,"outputTokens":64,"reasoningOutputTokens":0,"totalTokens":15573},"modelContextWindow":258400}}}`
	usageCycle2 = `{"method":"thread/tokenUsage/updated","params":{"threadId":"th_1","turnId":"tu_1","tokenUsage":{"last":{"inputTokens":15611,"cachedInputTokens":15360,"cacheWriteInputTokens":0,"outputTokens":40,"reasoningOutputTokens":0,"totalTokens":15651},"total":{"inputTokens":31120,"cachedInputTokens":26624,"cacheWriteInputTokens":0,"outputTokens":104,"reasoningOutputTokens":0,"totalTokens":31224},"modelContextWindow":258400}}}`
	usageCycle3 = `{"method":"thread/tokenUsage/updated","params":{"threadId":"th_1","turnId":"tu_1","tokenUsage":{"last":{"inputTokens":15690,"cachedInputTokens":15488,"cacheWriteInputTokens":0,"outputTokens":42,"reasoningOutputTokens":0,"totalTokens":15732},"total":{"inputTokens":46810,"cachedInputTokens":42112,"cacheWriteInputTokens":0,"outputTokens":146,"reasoningOutputTokens":0,"totalTokens":46956},"modelContextWindow":258400}}}`
	usageCycle4 = `{"method":"thread/tokenUsage/updated","params":{"threadId":"th_1","turnId":"tu_1","tokenUsage":{"last":{"inputTokens":15807,"cachedInputTokens":11264,"cacheWriteInputTokens":0,"outputTokens":5,"reasoningOutputTokens":0,"totalTokens":15812},"total":{"inputTokens":62617,"cachedInputTokens":53376,"cacheWriteInputTokens":0,"outputTokens":151,"reasoningOutputTokens":0,"totalTokens":62768},"modelContextWindow":258400}}}`

	// Second turn on the SAME thread — codex's `total` keeps climbing across
	// turns (80872), so nothing per-turn can be read off it directly.
	usageTurn2 = `{"method":"thread/tokenUsage/updated","params":{"threadId":"th_1","turnId":"tu_2","tokenUsage":{"last":{"inputTokens":18048,"cachedInputTokens":15616,"cacheWriteInputTokens":0,"outputTokens":56,"reasoningOutputTokens":0,"totalTokens":18104},"total":{"inputTokens":80665,"cachedInputTokens":68992,"cacheWriteInputTokens":0,"outputTokens":207,"reasoningOutputTokens":0,"totalTokens":80872},"modelContextWindow":258400}}}`

	turnCompleted1 = `{"method":"turn/completed","params":{"threadId":"th_1","turn":{"id":"tu_1","status":"completed"}}}`
	turnCompleted2 = `{"method":"turn/completed","params":{"threadId":"th_1","turn":{"id":"tu_2","status":"completed"}}}`
)

// openUsageTurn arms a turn and returns a pointer to the result it completes with.
func openUsageTurn(b *Backend) **delegator.TurnResult {
	got := new(*delegator.TurnResult)
	b.turnMu.Lock()
	b.turnActive = true
	b.turnEvents = &delegator.TurnEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { *got = r },
	}
	b.turnMu.Unlock()
	return got
}

// TestTokenUsage_MultiCycleTurnSumsEveryCycle is the #1855 regression: codex
// fires thread/tokenUsage/updated once per API CYCLE, and foci used to
// overwrite its stash on each one, delivering the last cycle alone as the
// whole turn (5 output tokens for a turn that spent 151).
func TestTokenUsage_MultiCycleTurnSumsEveryCycle(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.mu.Lock()
	b.model = "gpt-5.1-codex-max"
	b.mu.Unlock()

	got := openUsageTurn(b)
	for _, n := range []string{usageCycle1, usageCycle2, usageCycle3, usageCycle4} {
		b.dispatch([]byte(n))
	}
	b.dispatch([]byte(turnCompleted1))

	r := *got
	if r == nil || r.Usage == nil {
		t.Fatal("turn/completed delivered no usage")
	}
	u := r.Usage

	// Per-turn sums: every cycle counted exactly once.
	if u.Turn == nil {
		t.Fatal("Usage.Turn is nil — the per-turn accumulator was not delivered")
	}
	// 4245+251+202+4543 input (each cycle cached-subtracted), 64+40+42+5
	// output, 11264+15360+15488+11264 cache read.
	if u.Turn.Input != 9241 || u.Turn.Output != 151 || u.Turn.CacheRead != 53376 || u.Turn.CacheWrite != 0 {
		t.Errorf("Usage.Turn = %+v, want input=9241 output=151 cacheRead=53376 cacheWrite=0", *u.Turn)
	}
	// Control against codex's OWN running total for the final cycle
	// (input 62617, cached 53376, output 151): foci stores input
	// cached-subtracted, so the two must recombine exactly.
	if sum := u.Turn.Input + u.Turn.CacheRead; sum != 62617 {
		t.Errorf("turn input+cacheRead = %d, want codex's own total.inputTokens 62617", sum)
	}

	// OutputTokens is the turn sum (the one un-suffixed field that is summed).
	if u.OutputTokens != 151 {
		t.Errorf("OutputTokens = %d, want the turn sum 151 (last cycle alone is 5)", u.OutputTokens)
	}
	// Input/cache stay the FINAL cycle's context fill — summing them would
	// report several times the real occupancy to compaction.
	if u.InputTokens != 4543 || u.CacheReadInputTokens != 11264 {
		t.Errorf("fill = in %d cacheRead %d, want the last cycle's 4543/11264",
			u.InputTokens, u.CacheReadInputTokens)
	}

	// Cost is priced from the accumulator, so it must be non-zero for a
	// turn that spent tokens on a priced model.
	if u.CalculatedCostUSD == nil {
		t.Fatal("CalculatedCostUSD is nil — the turn was not priced")
	}
	if *u.CalculatedCostUSD <= 0 {
		t.Errorf("CalculatedCostUSD = %v, want > 0 (model %q)", *u.CalculatedCostUSD, r.Model)
	}
}

// TestGetContextWindow_ReportsLastCycleNotTurnSum pins the other half of the
// split: the accumulator must not leak into context occupancy, which is a
// snapshot of the final cycle's fill and is what drives compaction.
func TestGetContextWindow_ReportsLastCycleNotTurnSum(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	openUsageTurn(b)
	for _, n := range []string{usageCycle1, usageCycle2, usageCycle3, usageCycle4} {
		b.dispatch([]byte(n))
	}

	cw, err := b.GetContextWindow(context.Background())
	if err != nil {
		t.Fatalf("GetContextWindow: %v", err)
	}
	if cw.MaxTokens != 258400 {
		t.Errorf("MaxTokens = %d, want codex's reported 258400", cw.MaxTokens)
	}
	// Last cycle only: 4543 input + 5 output. The turn sum (9241+151) would
	// be a 2x overstatement of occupancy and compact the session early.
	if cw.TotalTokens != 4548 {
		t.Errorf("TotalTokens = %d, want the last cycle's 4548 (turn sum would be 9392)", cw.TotalTokens)
	}
}

// TestTokenUsage_AccumulatorResetsOnNewTurn proves the second turn is not
// charged for the first. codex's own `total` is cumulative for the THREAD's
// lifetime (80872 by the second turn's first cycle), so a per-turn figure
// that read it — or an accumulator that never reset — would compound.
func TestTokenUsage_AccumulatorResetsOnNewTurn(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	got1 := openUsageTurn(b)
	for _, n := range []string{usageCycle1, usageCycle2, usageCycle3, usageCycle4} {
		b.dispatch([]byte(n))
	}
	b.dispatch([]byte(turnCompleted1))
	if r := *got1; r == nil || r.Usage == nil || r.Usage.Turn == nil || r.Usage.Turn.Output != 151 {
		t.Fatalf("turn 1 premise not met: %+v", r)
	}

	got2 := openUsageTurn(b)
	b.dispatch([]byte(usageTurn2))
	b.dispatch([]byte(turnCompleted2))

	r := *got2
	if r == nil || r.Usage == nil || r.Usage.Turn == nil {
		t.Fatal("turn 2 delivered no per-turn usage")
	}
	if r.Usage.Turn.Output != 56 || r.Usage.Turn.Input != 2432 || r.Usage.Turn.CacheRead != 15616 {
		t.Errorf("turn 2 Usage.Turn = %+v, want input=2432 output=56 cacheRead=15616 (turn 1 not carried over)",
			*r.Usage.Turn)
	}
}
