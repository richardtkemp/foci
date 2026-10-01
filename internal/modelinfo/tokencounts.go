package modelinfo

import "time"

// TokenCounts is one pre-ledger api.db row's four billable token classes (one
// call's, or one turn's summed), plus the count of server-side web searches,
// which are billed per call rather than per token (#1913). The ledger
// migration reads the rows written before the per-call ledger in this shape
// (#1854, #2111); everything booked since is in classes (Tokens).
type TokenCounts struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`

	// WebSearches is the number of server-side web searches. Omitted from JSON
	// when zero so every row written before #1913 reads back unchanged.
	WebSearches int `json:"web_searches,omitempty"`
}

// Add returns t with o's counts added, class by class.
func (t TokenCounts) Add(o TokenCounts) TokenCounts {
	return TokenCounts{
		Input:      t.Input + o.Input,
		Output:     t.Output + o.Output,
		CacheRead:  t.CacheRead + o.CacheRead,
		CacheWrite: t.CacheWrite + o.CacheWrite,

		WebSearches: t.WebSearches + o.WebSearches,
	}
}

// Tokens maps t onto the class vocabulary. Its cache writes carry no TTL, so
// they go to writeClass — the caller says what it knows about them.
func (t TokenCounts) Tokens(writeClass Class) Tokens {
	return Tokens{
		ClassInput:     t.Input,
		ClassOutput:    t.Output,
		ClassCacheRead: t.CacheRead,
		writeClass:     t.CacheWrite,
		ClassWebSearch: t.WebSearches,
	}
}

// CostAsOf prices t for model at time at, its cache writes as TTL-unknown
// (ClassCacheWrite) — so a stored turn total re-priced through it lands on
// the CalculatedCostUSD it was recorded beside.
func (t TokenCounts) CostAsOf(model string, at time.Time) float64 {
	usd, _ := CostAsOf(model, at, t.Tokens(ClassCacheWrite))
	return usd
}
