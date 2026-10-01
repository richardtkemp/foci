package accounting

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/log"
	"foci/internal/modelinfo"
)

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-9 }

func sessTurn(name string) string { return "sess@" + name }

var startT1 = time.Date(2026, 9, 13, 14, 41, 35, 0, time.UTC)

// BookLegacy writes one row in the pre-ledger shape — a turn's parent share or
// one subagent's share, priced by a backend's old turn-level path — as a
// LEGACY call, through the migration's own converter, exactly as the cutover
// migrated the pre-ledger api.db (#2111 P2). No backend has booked this way
// since the Claude Code switch (#2115), so it lives here, in test code (#2145):
// it is how this package's tests put the migrated history every live ledger
// holds into a test ledger, so the readers' tests cover legacy calls.

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

// TestBookLegacyUnnamedSubagent: a share whose usage arrived before anything
// named its subagent is still a subagent's call, never the session's own
// thread (actor ""), and each is booked on its own.
func TestBookLegacyUnnamedSubagent(t *testing.T) {
	l, _ := openLedger(t)
	for i := 0; i < 2; i++ {
		c := modelinfo.TokenCounts{Output: 10}
		cost := 0.5
		if err := l.BookLegacy(LegacyRow{
			At: startT1, Session: "sess", Model: "claude-opus-5", TurnID: sessTurn("T1"),
			Subagent: true, Turn: &c, CalculatedCostUSD: &cost,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM api_calls WHERE actor = ?`, UnnamedSubagent).Scan(&n); err != nil || n != 2 {
		t.Errorf("unnamed subagent calls = %d (%v), want 2", n, err)
	}
}

// TestBookLegacyStoresTheBackendReport: a CC parent row's provided cost is
// CC's cumulative per-process figure; it is kept as a backend report (R5),
// never as the call's cost.
func TestBookLegacyStoresTheBackendReport(t *testing.T) {
	l, _ := openLedger(t)
	provided, calc := 12.5, 0.25
	c := modelinfo.TokenCounts{Input: 10, Output: 20}
	if err := l.BookLegacy(LegacyRow{
		At: startT1, Backend: BackendCCStream, Session: "sess", Model: "claude-opus-5", TurnID: sessTurn("T1"),
		Turn: &c, ProvidedCostUSD: &provided, CalculatedCostUSD: &calc,
	}); err != nil {
		t.Fatal(err)
	}
	var grain string
	var reported float64
	if err := l.db.QueryRow(`SELECT grain, cost_usd FROM backend_reports WHERE backend = 'ccstream'`).Scan(&grain, &reported); err != nil ||
		grain != GrainCumulative || reported != provided {
		t.Errorf("report = %s $%v (%v), want cumulative $12.5", grain, reported, err)
	}
	var cost float64
	var basis string
	if err := l.db.QueryRow(`SELECT cost_usd, cost_basis FROM call_costs`).Scan(&cost, &basis); err != nil ||
		cost != calc || basis != CostBasisRecorded {
		t.Errorf("call cost = $%v on %s (%v), want the recorded $0.25", cost, basis, err)
	}
}

// TestReadJSONLKeepsALegacyCallsRecordedFigure: api.jsonl written before the
// Claude Code switch (#2115) holds legacy calls, booked there with no api.db.
// ReadJSONL takes such a call's cost from its recorded figure, never its
// counts, and gives the counts no class costs, as Calls does from api.db.
func TestReadJSONLKeepsALegacyCallsRecordedFigure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.jsonl")
	f := openJSONL(t, path)
	calc := 0.75
	c := modelinfo.TokenCounts{Input: 10, Output: 20}
	var l *Ledger // no api.db: the call is only observed
	if err := l.BookLegacy(LegacyRow{
		At: startT1, Backend: BackendCCStream, Session: "sess", Model: "claude-opus-5", TurnID: sessTurn("T1"),
		Turn: &c, CalculatedCostUSD: &calc,
	}); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	rows := ReadJSONL(path)
	if len(rows) != 1 {
		t.Fatalf("api.jsonl = %+v, want one line", rows)
	}
	r := rows[0]
	if r.Kind != KindLegacy || r.CostBasis != CostBasisRecorded || r.Cost() != calc {
		t.Errorf("line = %s on %s at $%v, want a legacy call at its recorded $0.75", r.Kind, r.CostBasis, r.Cost())
	}
	if out := r.Classes[modelinfo.ClassOutput]; out.Count != 20 || out.CostUSD != nil {
		t.Errorf("output class = %+v, want its 20 counted but unpriced", out)
	}
}

// openJSONL makes path the api.jsonl for the test.
func openJSONL(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	log.SetAPIWriter(f)
	t.Cleanup(func() { log.SetAPIWriter(nil) })
	return f
}
