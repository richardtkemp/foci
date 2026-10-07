package accounting

import (
	"bufio"
	"encoding/json"
	"os"
	"time"

	"foci/internal/log"
	"foci/internal/modelinfo"
)

// jsonlLine is one api.jsonl line: one booked call (#2111 §6 "api.jsonl keeps
// one line per call, append-only"). The file is a mirror of api.db for the
// processes that run without one, and ts and cost_usd keep the names the
// pre-ledger lines used, so line readers outside foci still find them.
type jsonlLine struct {
	TS          time.Time        `json:"ts"`
	ID          int64            `json:"id,omitempty"`
	Key         string           `json:"call_key,omitempty"`
	Backend     string           `json:"backend"`
	Provider    string           `json:"provider,omitempty"`
	Model       string           `json:"model"`
	Session     string           `json:"session"`
	AgentID     string           `json:"agent_id,omitempty"`
	TurnID      string           `json:"turn_id,omitempty"`
	Actor       string           `json:"actor,omitempty"`
	Kind        string           `json:"kind"`
	Finality    string           `json:"finality"`
	ClassMethod string           `json:"class_method"`
	CostBasis   string           `json:"cost_basis"`
	Tokens      modelinfo.Tokens `json:"tokens"`
	// CostUSD is the call's cost as Book priced it; null when a billed class
	// was unpriced. ReadJSONL re-prices counts rather than trusting it.
	CostUSD     *float64       `json:"cost_usd"`
	ContextFill int            `json:"context_fill,omitempty"`
	Purpose     string         `json:"purpose,omitempty"`
	StopReason  string         `json:"stop_reason,omitempty"`
	SessionFile string         `json:"session_file,omitempty"`
	SessionLine int            `json:"session_line,omitempty"`
	Detail      map[string]any `json:"detail,omitempty"`
}

// appendJSONL writes one booking to api.jsonl.
func appendJSONL(b Booking) {
	log.AppendAPILine(jsonlLine{
		TS: b.BilledAt, ID: b.ID, Key: b.Key,
		Backend: b.Backend, Provider: b.Provider, Model: b.Model,
		Session: b.Session, AgentID: b.AgentID, TurnID: b.TurnID, Actor: b.Actor,
		Kind: b.Kind, Finality: b.Finality, ClassMethod: b.ClassMethod, CostBasis: b.basis(),
		Tokens: nonZero(b.Tokens), CostUSD: b.CostUSD, ContextFill: b.Fill, Purpose: b.Purpose,
		StopReason: b.StopReason, SessionFile: b.SessionFile, SessionLine: b.SessionLine,
		Detail: b.Detail,
	})
}

// ReadJSONL reads an api.jsonl file into call rows, in file order. It is the
// fallback for a process with no api.db (unit tests, a gateway run without
// api_db). Lines that do not parse — including any written before the ledger
// — are skipped.
func ReadJSONL(path string) []CallRow {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []CallRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var l jsonlLine
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Kind == "" || l.Backend == "" {
			continue
		}
		r := CallRow{
			ID: l.ID, BilledAt: l.TS, Backend: l.Backend, Provider: l.Provider, Model: l.Model,
			Session: l.Session, AgentID: l.AgentID, TurnID: l.TurnID, Actor: l.Actor,
			Kind: l.Kind, CostBasis: l.CostBasis, Fill: l.ContextFill,
			Purpose: l.Purpose, Classes: map[modelinfo.Class]ClassCost{},
		}
		// Priced at read time, as the views price a call: its counts at the
		// rates in effect when it was billed, or its recorded figure. The
		// line's cost_usd is what Book priced then, kept for line readers.
		c := Call{Model: l.Model, BilledAt: l.TS, Tokens: l.Tokens, CostBasis: l.CostBasis}
		if l.CostBasis == CostBasisRecorded {
			c.LegacyCalculatedCostUSD = l.CostUSD
		}
		r.CostUSD = c.cost()
		for class, n := range l.Tokens {
			cc := ClassCost{Count: n}
			if r.CostBasis != CostBasisRecorded && l.Model != "" {
				if usd, ok := modelinfo.CostAsOfPrompt(l.Model, l.TS, modelinfo.Tokens{class: n}, c.PricingPrompt()); ok {
					cc.CostUSD = &usd
				}
			}
			r.Classes[class] = cc
		}
		out = append(out, r)
	}
	return out
}
