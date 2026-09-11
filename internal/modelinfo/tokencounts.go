package modelinfo

import "time"

// TokenCounts is one API call's (or one turn's summed) four billable token
// classes. It is the unit that Cost/CostAsOf price, held as a value so the
// same shape can be accumulated across a delegated backend's ask cycles,
// carried on a TurnUsage/Usage/APIEntry, and persisted to api.db (#1854).
//
// Scope is the caller's contract, not the type's: a backend that populates a
// TurnUsage.Turn from this promises the counts are the SUM of every API
// cycle's own tokens within the turn — the figure CalculatedCostUSD was priced
// from — never the final cycle's context fill, which is a different quantity
// living in the un-suffixed TurnUsage fields.
type TokenCounts struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
}

// Add returns t with o's counts added, class by class.
func (t TokenCounts) Add(o TokenCounts) TokenCounts {
	return TokenCounts{
		Input:      t.Input + o.Input,
		Output:     t.Output + o.Output,
		CacheRead:  t.CacheRead + o.CacheRead,
		CacheWrite: t.CacheWrite + o.CacheWrite,
	}
}

// CostAsOf prices t for model at time at — the same function every other
// caller prices with, so a stored turn total re-priced through it lands on
// the CalculatedCostUSD it was recorded beside.
func (t TokenCounts) CostAsOf(model string, at time.Time) float64 {
	return CostAsOf(model, at, t.Input, t.Output, t.CacheRead, t.CacheWrite)
}

// SubClamped returns t minus o class by class, with any class that would go
// negative pinned at zero. ok is false when a class was pinned.
//
// It exists for splitting an authoritative total into a parent share and the
// subagent shares taken out of it (#1880 phase C). A negative parent share can
// only mean the subagent bucket counted something the authoritative total does
// not, so the honest response is to stop at zero and let the caller say so —
// the alternative, a negative token count, prices as a CREDIT and would quietly
// reduce the bill.
func (t TokenCounts) SubClamped(o TokenCounts) (TokenCounts, bool) {
	ok := true
	clamp := func(a, b int) int {
		if a-b < 0 {
			ok = false
			return 0
		}
		return a - b
	}
	return TokenCounts{
		Input:      clamp(t.Input, o.Input),
		Output:     clamp(t.Output, o.Output),
		CacheRead:  clamp(t.CacheRead, o.CacheRead),
		CacheWrite: clamp(t.CacheWrite, o.CacheWrite),
	}, ok
}

// SubagentCost is one subagent's priced share of a turn, on ONE model.
//
// Keyed by both because a row carries one model in its model column while the
// spend is attributed to the agent — and a subagent that spawns its own
// children can touch more than one. AgentID is the Agent tool's tool_use id,
// which also names the transcript (.../subagents/agent-<id>.jsonl), so a row
// written from this traces back to the work that incurred it. It is empty for
// usage that arrived before its task_started named the agent; that still
// separates subagent spend from the parent's, which is the point.
type SubagentCost struct {
	AgentID string `json:"agent_id"`
	Model   string `json:"model"`

	// TurnID is the turn that SPAWNED this subagent, which is not always the
	// turn whose books this share lands in: a background subagent can outlive
	// its parent by half an hour, and its spend belongs to the work that
	// started it (#1880 phase C). Empty when the backend could not name one, in
	// which case the writer falls back to the turn it is writing.
	TurnID string `json:"turn_id,omitempty"`

	Counts  TokenCounts `json:"counts"`
	CostUSD float64     `json:"cost_usd"`
}
