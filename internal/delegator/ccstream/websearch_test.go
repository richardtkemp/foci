package ccstream

import (
	"encoding/json"
	"math"
	"testing"

	"foci/internal/delegator"
)

// #1913: web search is billed per CALL ($0.01), so no token class carries it,
// and foci priced every searched turn exactly $0.01 per search LOW.
//
// These are the three result messages of a live probe (2026-09-23, CC fed three
// turns in ONE process: search, no search, search), trimmed to the fields
// pricing reads. Replayed rather than hand-built because the facts that matter
// are CC's, not ours: the count is cumulative per process (1, 1, 2), and it
// lands on the model that RAN the search — haiku, CC's search sub-call — not
// the turn's model. A hand-built fixture would encode whatever we assumed.
var webSearchProbeResults = []string{
	`{"type":"result","subtype":"success","total_cost_usd":0.1084616,"modelUsage":{
	  "claude-sonnet-5":{"inputTokens":6,"outputTokens":201,"cacheReadInputTokens":86473,"cacheCreationInputTokens":16971,"webSearchRequests":0,"costUSD":0.0872006},
	  "claude-haiku-4-5-20251001":{"inputTokens":10531,"outputTokens":146,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"webSearchRequests":1,"costUSD":0.021261000000000002}}}`,
	`{"type":"result","subtype":"success","total_cost_usd":0.116082,"modelUsage":{
	  "claude-sonnet-5":{"inputTokens":8,"outputTokens":204,"cacheReadInputTokens":122085,"cacheCreationInputTokens":17087,"webSearchRequests":0,"costUSD":0.094821},
	  "claude-haiku-4-5-20251001":{"inputTokens":10531,"outputTokens":146,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"webSearchRequests":1,"costUSD":0.021261000000000002}}}`,
	`{"type":"result","subtype":"success","total_cost_usd":0.15714840000000002,"modelUsage":{
	  "claude-sonnet-5":{"inputTokens":12,"outputTokens":328,"cacheReadInputTokens":193627,"cacheCreationInputTokens":17939,"webSearchRequests":0,"costUSD":0.11378540000000001},
	  "claude-haiku-4-5-20251001":{"inputTokens":21963,"outputTokens":280,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"webSearchRequests":2,"costUSD":0.043363}}}`,
}

// TestOnResult_PricesWebSearchesToMatchCC replays the probe and asserts foci's
// figure equals CC's on every turn. On this probe the token classes already
// agree to the micro-dollar, so the ONLY thing that can open a gap is the
// searches — before #1913 turns 1 and 3 read exactly $0.010000 low, the
// signature in the divergence warning that opened the ticket.
func TestOnResult_PricesWebSearchesToMatchCC(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	var res *delegator.TurnResult

	wantSearches := []int{1, 0, 1}
	for i, raw := range webSearchProbeResults {
		var msg ResultMessage
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			t.Fatalf("turn %d: decode: %v", i+1, err)
		}
		// One foci turn per probe turn, on the SAME backend — the cumulative
		// counters only mean anything within one process.
		res = nil
		applyHandler(b, &testHandler{OnTurnComplete: func(r *delegator.TurnResult) { res = r }})
		b.mu.Lock()
		b.lastModel = "claude-sonnet-5"
		b.mu.Unlock()

		b.OnResult(&msg)

		b.turnMu.Lock()
		calc, provided := b.turnCalcCostUSD, b.turnProvidedUSD
		b.turnMu.Unlock()

		if math.Abs(calc-provided) > 1e-9 {
			t.Errorf("turn %d: foci priced $%.6f, CC reported $%.6f (gap $%.6f) — web searches unpriced?",
				i+1, calc, provided, provided-calc)
		}
		if res == nil || res.Usage == nil || res.Usage.Turn == nil {
			t.Fatalf("turn %d: no Usage.Turn", i+1)
		}
		if got := res.Usage.Turn.WebSearches; got != wantSearches[i] {
			t.Errorf("turn %d: Turn.WebSearches = %d, want %d (a per-turn DELTA of CC's cumulative count)",
				i+1, got, wantSearches[i])
		}
	}
}
