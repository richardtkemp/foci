package ccstream

import (
	"bytes"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/modelinfo"
)

// The #1854 invariant. One api.db row carries CalculatedCostUSD summed across
// a turn's ask cycles beside token columns holding only the LAST cycle's
// context fill, so pricing a row from its own columns recovered ~20% of its
// recorded cost over 14 days ($402 of $2,040) without any error. Usage.Turn is
// the per-cycle sum the cost was actually priced from, and must re-price to
// exactly that cost — while the context-fill fields stay the last cycle's,
// because compaction reads them.
func TestOnResult_TurnCountsRepriceToCalculatedCost(t *testing.T) {
	t.Parallel()
	const model = "claude-opus-4-20250514"

	var completed *delegator.TurnResult
	b := &Backend{writer: NewWriter(nopWriteCloser{&bytes.Buffer{}})}
	b.typingFunc = func(bool) {}
	applyHandler(b, &testHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completed = r },
	})
	stateEvent(b, "running")

	// CC's ModelUsage is CUMULATIVE over the process; each cycle's own figures
	// are the delta. Two cycles with distinct, non-overlapping shapes so a
	// last-only or a double-counted sum both land visibly wrong.
	cycles := []struct {
		last TokenUsage // the final assistant message's usage = context fill
		cum  ModelUsage // cumulative, as CC reports it
	}{
		{
			last: TokenUsage{InputTokens: 7, CacheReadInputTokens: 80000, CacheCreationInputTokens: 41000},
			cum:  ModelUsage{InputTokens: 7, OutputTokens: 800, CacheReadInputTokens: 80000, CacheCreationInputTokens: 41000, ContextWindow: 200000},
		},
		{
			last: TokenUsage{InputTokens: 3, CacheReadInputTokens: 121000, CacheCreationInputTokens: 300},
			cum:  ModelUsage{InputTokens: 10, OutputTokens: 815, CacheReadInputTokens: 201000, CacheCreationInputTokens: 41300, ContextWindow: 200000},
		},
	}
	for _, c := range cycles {
		b.mu.Lock()
		b.lastModel = model
		last := c.last
		b.lastUsage = &last
		b.mu.Unlock()
		b.OnResult(&ResultMessage{
			Subtype:    "success",
			Result:     "reply",
			ModelUsage: map[string]ModelUsage{model: c.cum},
		})
	}
	stateEvent(b, "idle")

	if completed == nil || completed.Usage == nil {
		t.Fatal("no result usage")
	}
	u := completed.Usage
	if u.Turn == nil {
		t.Fatal("Usage.Turn = nil, want the per-cycle sum")
	}
	if u.CalculatedCostUSD == nil {
		t.Fatal("Usage.CalculatedCostUSD = nil")
	}

	// Sum of the deltas = the final cumulative figure, by construction.
	want := modelinfo.TokenCounts{Input: 10, Output: 815, CacheRead: 201000, CacheWrite: 41300}
	if *u.Turn != want {
		t.Errorf("Usage.Turn = %+v, want %+v (sum of both cycles' deltas)", *u.Turn, want)
	}

	// The context-fill fields must still be the LAST cycle's, not the sum.
	if u.InputTokens != 3 || u.CacheReadInputTokens != 121000 || u.CacheCreationInputTokens != 300 {
		t.Errorf("context fill = in=%d cr=%d cw=%d, want last cycle's 3/121000/300",
			u.InputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens)
	}

	// The invariant api.db can now check for itself: Turn priced through the
	// same function lands on CalculatedCostUSD.
	got := u.Turn.CostAsOf(completed.Model, time.Now())
	if diff := got - *u.CalculatedCostUSD; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("Turn re-prices to $%.6f but CalculatedCostUSD = $%.6f (diff $%.6f)",
			got, *u.CalculatedCostUSD, diff)
	}
}

// Without a ModelUsage delta nothing was summed, and the row must say so with
// NULL rather than a zero that prices as free.
func TestOnResult_TurnCountsNilWithoutModelUsage(t *testing.T) {
	t.Parallel()
	var completed *delegator.TurnResult
	b := &Backend{writer: NewWriter(nopWriteCloser{&bytes.Buffer{}})}
	b.typingFunc = func(bool) {}
	applyHandler(b, &testHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completed = r },
	})
	stateEvent(b, "running")
	b.OnResult(&ResultMessage{Subtype: "success", Result: "ok", Usage: TokenUsage{OutputTokens: 5}})
	stateEvent(b, "idle")

	if completed == nil || completed.Usage == nil {
		t.Fatal("no result usage")
	}
	if completed.Usage.Turn != nil {
		t.Errorf("Usage.Turn = %+v, want nil when no ModelUsage delta was seen", *completed.Usage.Turn)
	}
}
