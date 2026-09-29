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
