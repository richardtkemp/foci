package accounting

import (
	"database/sql"
	"fmt"
	"time"

	"foci/internal/modelinfo"
)

// BookLegacy writes one row in the pre-ledger shape — a turn's parent share or
// one subagent's share, priced by a backend's old turn-level path — as a
// LEGACY call, through the migration's own converter, exactly as the cutover
// migrated the pre-ledger api.db (#2111 P2). No backend books this way since
// the Claude Code switch (#2115): every delegated backend books its own calls.
// It stays as the one way to put the migrated history every live ledger holds
// into a test ledger, so the readers' tests (/cost, the shadow comparison)
// cover legacy calls; nothing in the gateway calls it (the Makefile's deadcode
// gate exempts this file).

// LegacyRow is one row in the pre-ledger shape: a turn's parent share, or one
// subagent's share, priced by its backend's pre-ledger cost path.
type LegacyRow struct {
	At time.Time
	// Backend is the ledger backend that wrote the row (DelegatorBackend); ""
	// infers it from the model, as the migration does.
	Backend                  string
	Provider, Session, Model string
	// Fill is a parent row's final-cycle context fill; zero on a subagent
	// share, which has none of its own.
	Fill LegacyFill
	// Turn is the turn-summed counts CalculatedCostUSD was priced from; nil
	// when the backend measured none, and Fill then stands in, as the
	// migration does for rows written before #1854.
	Turn *modelinfo.TokenCounts
	// ProvidedCostUSD is the backend's own figure (CC: cumulative per process;
	// opencode: the turn's), stored as a backend report. CalculatedCostUSD is
	// the pre-ledger path's priced figure, which the views take.
	ProvidedCostUSD, CalculatedCostUSD *float64
	DurationMS                         int64
	StopReason, SessionFile            string
	TurnID, AgentID, Purpose           string
	// Subagent marks a subagent's share, and SubagentID names the subagent
	// (the Agent tool_use id) — "" when its usage arrived before anything
	// named it.
	Subagent   bool
	SubagentID string
}

// v1 converts the row into the migration's row shape, with its cache writes
// classed and its cost basis decided the way the migration decides them.
func (r LegacyRow) v1() *v1Row {
	v := &v1Row{
		ts: r.At, provider: r.Provider, session: r.Session, model: r.Model, fill: r.Fill,
		durationMS: r.DurationMS, stopReason: r.StopReason, callType: "delegated_turn",
		sessionFile: r.SessionFile, turnID: r.TurnID, agentID: r.AgentID,
		subagentID: r.SubagentID, purpose: r.Purpose,
	}
	if r.Subagent || r.SubagentID != "" {
		v.callType = "subagent_turn"
	}
	if r.ProvidedCostUSD != nil {
		v.costUSD = sql.NullFloat64{Float64: *r.ProvidedCostUSD, Valid: true}
	}
	if r.CalculatedCostUSD != nil {
		v.calcUSD = sql.NullFloat64{Float64: *r.CalculatedCostUSD, Valid: true}
	}
	if r.Turn != nil {
		v.turnTotals, v.priced = true, *r.Turn
	} else {
		v.priced = modelinfo.TokenCounts{Input: r.Fill.Input, Output: r.Fill.Output,
			CacheRead: r.Fill.CacheRead, CacheWrite: r.Fill.CacheWrite}
	}
	// Every live row names its turn; one that somehow does not gets a turn of
	// its own rather than the migration's "legacy:<id>", which needs a row id.
	if v.turnID == "" {
		v.turnID = fmt.Sprintf("%s@%d", r.Session, r.At.UnixNano())
	}
	v.backend = r.Backend
	if v.backend == "" {
		v.backend = inferBackend(v)
	}
	classifyByKind(v)
	v.costBasis = legacyCostBasis(v)
	return v
}

// BookLegacy books one pre-ledger row as a legacy call, with its turn and its
// backend's own reported figure. On a nil ledger (no api.db) the call is only
// observed — written to api.jsonl and handed to BookedHook — as Record does.
func (l *Ledger) BookLegacy(r LegacyRow) error {
	v := r.v1()
	if l == nil {
		c := legacyCall(v)
		if err := c.validate(); err != nil {
			return err
		}
		fill := 0
		if !v.isSubagent() {
			fill = v.fill.Input + v.fill.CacheRead + v.fill.CacheWrite
		}
		observe(Booking{Call: c, CostUSD: c.cost(), Purpose: v.purpose, Fill: fill,
			TurnSource: legacyTurn(v.turnKey(), []*v1Row{v}).Source})
		return nil
	}
	return l.Update(func(tx *Tx) error { return tx.bookLegacy(v) })
}

func (tx *Tx) bookLegacy(v *v1Row) error {
	t := legacyTurn(v.turnKey(), []*v1Row{v})
	if v.isSubagent() {
		// A subagent's share is booked to the turn that SPAWNED it (#1880),
		// which is usually an earlier turn its own parent row recorded. The
		// share must never rewrite that turn's facts — its end in particular,
		// which the #1918 correction resolves against — so it only creates the
		// turn when there is none.
		var n int
		if err := tx.tx.QueryRow(`SELECT COUNT(*) FROM turns WHERE turn_id = ?`, t.TurnID).Scan(&n); err != nil {
			return fmt.Errorf("ledger: find turn %q: %w", t.TurnID, err)
		}
		if n == 0 {
			if err := tx.RecordTurn(t); err != nil {
				return err
			}
		}
	} else if err := tx.RecordTurn(t); err != nil {
		return err
	}
	if _, err := tx.Book(legacyCall(v)); err != nil {
		return err
	}
	if rp, ok := legacyReport(v); ok {
		return tx.Report(rp)
	}
	return nil
}
