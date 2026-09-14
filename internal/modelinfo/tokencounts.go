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

// CostCorrection moves spend that was booked to the wrong row after that row
// was already written (#1918, following #1909).
//
// A subagent's spend reaches foci by tailing its transcript, which can lag the
// billing by anything from a poll interval to the 60s the tailer will wait for
// the file to exist. Spend still undelivered when a turn's result closes is
// nonetheless inside that turn's authoritative ModelUsage, so the parent share
// — computed as ModelUsage minus what had been delivered — silently absorbs it.
// The turn's TOTAL is right; its split is not.
//
// The correction moves exactly that amount off the parent row of the turn that
// absorbed it and onto the subagent row of the turn that spawned the agent.
// Those are different turns whenever a background subagent outlives its parent,
// which is why both ids are carried rather than one.
//
// It is applied as an UPDATE of the two existing rows, never as a third signed
// row: a reader must get the truth from a plain lookup, without summing
// corrections (Dick, 2026-09-14 — "I don't want a correcting pair, I just want
// a single correct entry").
type CostCorrection struct {
	// BilledAt is when CC billed the spend — the transcript line's own
	// timestamp. The turn whose parent row absorbed it is resolved FROM THIS,
	// at apply time, by asking api_calls for the first turn of this session to
	// close at or after it. A turn is priced from the PREVIOUS result, so its
	// window runs from the previous turn's close to its own and the timeline
	// tiles with no gaps — idle time belongs to the turn that follows it.
	//
	// Deliberately not a resolved turn id. An earlier version carried one,
	// resolved against a 16-entry in-memory ring of turn windows; that ring was
	// a bounded cache of a mapping api_calls already holds durably, unbounded
	// and indexed, on the same connection this correction writes through. Its
	// bound was also wrong: measured over 26,836 turns, 2.4% of 30-minute
	// windows contain more than 16 turns and the worst holds 155.
	BilledAt time.Time `json:"billed_at"`
	// SubagentTurnID is the turn that SPAWNED the agent, under which phase C
	// files every one of its rows. Gains the amount.
	SubagentTurnID string `json:"subagent_turn_id"`

	AgentID string `json:"agent_id"`
	Model   string `json:"model"`

	Counts  TokenCounts `json:"counts"`
	CostUSD float64     `json:"cost_usd"`

	// StrandedUSD is the cache-write SURCHARGE this correction leaves behind on
	// the parent row, and it is reported rather than repaired (#1929).
	//
	// The parent absorbed these tokens as an unobserved residue, which splitFor
	// puts in the Unknown class and Unknown prices at the 1h rate ($10/MTok on
	// opus-5). The correction removes them at the subagent's OWN observed split,
	// which is 5m ($6.25). So $3.75 per million stays on a row that no longer has
	// the tokens, the #1854 re-price identity breaks for that row, and an
	// over-charge the divergence check flagged at pricing time is never repaired.
	//
	// It is NOT a rounding artefact: those really were 5m tokens billed at the 1h
	// rate. Repairing it properly means debiting the parent at the basis it was
	// charged and crediting the subagent at its own — two figures, and a turn
	// total that legitimately FALLS. That trades dollar-conservation for
	// token-conservation, which is the invariant this whole arc leans on, so it
	// is not a change to make before the size is known. This field is what makes
	// it knowable: #1920 reads it out of the log after deploy.
	StrandedUSD float64 `json:"stranded_usd,omitempty"`
}
