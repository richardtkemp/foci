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
