package accounting

import (
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"foci/internal/log"
	"foci/internal/modelinfo"
)

// This file books the rows of the delegated backends that have not switched
// to per-call booking yet (#2111 P2: the direct API switched first; opencode,
// codex and Claude Code follow, one change each). Their turn-level cost path —
// one parent row per turn, one row per subagent share, #1918 corrections —
// is booked here as LEGACY calls, through the migration's own converter, so a
// live pre-ledger row and a migrated one have the same shape and the views
// price both from their recorded figure. It is each such backend's ONLY
// booking path, and each backend's switch deletes its use of it; the last
// switch deletes the file.

var ledgerLog = log.NewComponentLogger("ledger")

// LegacyRow is one row as a not-yet-switched delegated backend writes it
// (agent.DelegatedTransport.LogUsage): a turn's parent share, or one
// subagent's share, priced by that backend's pre-ledger cost path.
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

// DelegatorBackend maps a registered delegator name (agent's
// DelegatedManager.BackendType) to its ledger backend, or "" when unknown.
func DelegatorBackend(name string) string {
	switch name {
	case "claude-code", "claude-code-tmux":
		return BackendCCStream
	case "opencode":
		return BackendOpencode
	case "codex":
		return BackendCodex
	}
	return ""
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

// AccumulateLegacySubagent folds one subagent share into THE legacy call for
// (turn, subagent, model), booking it on first sight (#1922). A subagent that
// outlives its parent turn is written once per turn it straddles, all under
// the spawning turn; folding keeps that one call, so "what did the delegation
// cost" is a lookup and a #1918 correction (which needs exactly one match)
// can reach it. It reports whether an existing call was extended.
func (l *Ledger) AccumulateLegacySubagent(r LegacyRow) (merged bool, err error) {
	v := r.v1()
	if l == nil || !v.isSubagent() || r.TurnID == "" || r.SubagentID == "" {
		return false, l.BookLegacy(r)
	}
	err = l.Update(func(tx *Tx) error {
		var id int64
		err := tx.tx.QueryRow(`SELECT id FROM api_calls
			WHERE kind = 'legacy' AND turn_id = ? AND actor = ? AND model = ?
			  AND json_extract(detail, '$.v1_call_type') = 'subagent_turn'
			ORDER BY id LIMIT 1`, v.turnID, v.subagentID, v.model).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return tx.bookLegacy(v)
		}
		if err != nil {
			return fmt.Errorf("ledger: find subagent call: %w", err)
		}
		merged = true
		if err := tx.addTokens(id, v.tokens()); err != nil {
			return err
		}
		if _, err := tx.tx.Exec(`UPDATE api_calls SET
				legacy_calculated_cost_usd = COALESCE(legacy_calculated_cost_usd, 0) + ?,
				detail = json_set(COALESCE(detail, '{}'), '$.duration_ms', ?)
			WHERE id = ?`, v.calcUSD.Float64, v.durationMS, id); err != nil {
			return fmt.Errorf("ledger: extend subagent call %d: %w", id, err)
		}
		// The observers see the instalment as its own call: api.jsonl and the
		// trace exporter are append-only, and their readers sum.
		b, err := tx.booking(legacyCall(v), id)
		if err != nil {
			return err
		}
		b.Instalment = true
		tx.booked = append(tx.booked, b)
		return nil
	})
	return merged, err
}

// addTokens adds counts to a booked call (a negative count subtracts). A class
// that reaches zero is deleted — call_tokens holds only positive counts — and
// one that would go negative is an error: a negative count prices as a credit.
func (tx *Tx) addTokens(id int64, t modelinfo.Tokens) error {
	stored, err := tx.tokens(id)
	if err != nil {
		return err
	}
	for _, class := range slices.Sorted(maps.Keys(t)) {
		n := t[class]
		if n == 0 {
			continue
		}
		switch sum := stored[class] + n; {
		case sum < 0:
			return fmt.Errorf("ledger: call %d would hold %d %s", id, sum, class)
		case sum == 0:
			_, err = tx.tx.Exec(`DELETE FROM call_tokens WHERE call_id = ? AND class = ?`, id, string(class))
		default:
			_, err = tx.tx.Exec(`INSERT INTO call_tokens (call_id, class, count) VALUES (?, ?, ?)
				ON CONFLICT (call_id, class) DO UPDATE SET count = excluded.count`, id, string(class), sum)
		}
		if err != nil {
			return fmt.Errorf("ledger: add %d %s to call %d: %w", n, class, id, err)
		}
	}
	return nil
}

// ApplyLegacyCorrections moves late-arriving subagent spend off the legacy
// parent call that absorbed it and onto the legacy subagent call that should
// have carried it (#1918, following #1909).
//
// WHY AN UPDATE AND NOT A THIRD ROW. A subagent's spend reaches foci by
// tailing its transcript, which lags billing. Spend still undelivered when a
// turn's result closes is nonetheless inside that turn's ModelUsage, so the
// parent share absorbs it. The total is right and the split is not; the
// requirement is that a plain lookup is already correct (Dick, 2026-09-14: "I
// don't want a correcting pair, I just want a single correct entry").
//
// ALL OR NOTHING, PER CORRECTION: one transaction each, both sides must match
// exactly one call, and the parent must be able to cover the move — a
// negative count prices as a credit, and a parent that cannot cover the move
// means the correction is wrong. A skipped correction costs only the
// attribution.
//
// TOKENS ARE CONSERVED, DOLLARS MAY FALL (#1929): both calls move by the same
// counts, but the parent gives up CostUSD + TTLSurchargeUSD (the 1h basis it
// absorbed the writes at) and the subagent gains CostUSD (its observed 5m
// basis). Its cache writes move from the parent's cache_write_1h — the class
// the kind rule gives a CC parent — to the subagent's cache_write_5m.
func (l *Ledger) ApplyLegacyCorrections(cs []modelinfo.CostCorrection) {
	if l == nil {
		return // no api.db: nothing was booked to correct
	}
	for _, c := range cs {
		if c.SubagentTurnID == "" || c.AgentID == "" || c.BilledAt.IsZero() {
			continue
		}
		var parentTurn string
		err := l.Update(func(tx *Tx) error {
			var err error
			parentTurn, err = tx.applyCorrection(c)
			return err
		})
		if err != nil {
			ledgerLog.Warnf("cost correction skipped (billed %s -> subagent %s on turn %s, $%.6f): %v",
				formatTime(c.BilledAt), c.AgentID, c.SubagentTurnID, c.CostUSD, err)
			continue
		}
		ledgerLog.Infof("cost correction applied: $%.6f debited from parent turn %s, "+
			"$%.6f credited to subagent %s on turn %s (%d cache-read, %d cache-write); "+
			"ttl_surcharge_removed=$%.6f (#1918, #1929)",
			c.CostUSD+c.TTLSurchargeUSD, parentTurn, c.CostUSD, c.AgentID, c.SubagentTurnID,
			c.Counts.CacheRead, c.Counts.CacheWrite, c.TTLSurchargeUSD)
		if CorrectionHook != nil {
			CorrectionHook(c, parentTurn)
		}
	}
}

// applyCorrection performs one correction, returning the parent turn it
// debited.
func (tx *Tx) applyCorrection(c modelinfo.CostCorrection) (string, error) {
	session, _, ok := strings.Cut(c.SubagentTurnID, "@")
	if !ok || session == "" {
		return "", fmt.Errorf("subagent turn id %q has no session prefix", c.SubagentTurnID)
	}
	// WHICH parent absorbed the spend: a turn is priced from the previous
	// result, so its window runs from the previous turn's close to its own and
	// the timeline tiles with no gaps. The turn that absorbed spend billed at t
	// is the first of the session to CLOSE at or after t — its end, never its
	// start (#1922: start-based resolution debited the NEXT turn for every
	// subagent billed while its turn ran). billed_at and ended_at are both UTC
	// fixed-width text, so they compare as times.
	var parentTurn string
	err := tx.tx.QueryRow(`SELECT t.turn_id FROM turns t
		WHERE t.session = ? AND t.ended_at >= ?
		  AND EXISTS (SELECT 1 FROM api_calls p WHERE p.turn_id = t.turn_id AND p.kind = 'legacy'
		              AND p.actor = '' AND json_extract(p.detail, '$.v1_call_type') = 'delegated_turn')
		ORDER BY t.started_at LIMIT 1`, session, formatTime(c.BilledAt)).Scan(&parentTurn)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("no turn of session %s closed at or after %s", session, formatTime(c.BilledAt))
	}
	if err != nil {
		return "", fmt.Errorf("resolve parent turn: %w", err)
	}

	parent, err := tx.oneLegacyCall(legacyParentQuery, "parent call for turn "+parentTurn, parentTurn)
	if err != nil {
		return "", err
	}
	sub, err := tx.oneLegacyCall(legacyShareQuery,
		"subagent call for turn "+c.SubagentTurnID+" agent "+c.AgentID, c.SubagentTurnID, c.AgentID, c.Model)
	if err != nil {
		return "", err
	}

	debit := c.CostUSD + c.TTLSurchargeUSD
	from := modelinfo.Tokens{
		modelinfo.ClassInput: c.Counts.Input, modelinfo.ClassOutput: c.Counts.Output,
		modelinfo.ClassCacheRead: c.Counts.CacheRead, modelinfo.ClassCacheWrite1h: c.Counts.CacheWrite,
	}
	// A parent whose counts are the final cycle's fill, not turn totals, was
	// never measured, and subtracting from it would assert a measurement that
	// was never made.
	if !parent.turnTotals || !parent.recorded.Valid {
		return "", fmt.Errorf("parent call for turn %s has unmeasured counts", parentTurn)
	}
	for class, n := range from {
		if parent.tokens[class] < n {
			return "", fmt.Errorf("parent call for turn %s cannot cover the correction (has %v $%.6f, needs %v $%.6f)",
				parentTurn, parent.tokens, parent.recorded.Float64, nonZero(from), debit)
		}
	}
	if parent.recorded.Float64 < debit {
		return "", fmt.Errorf("parent call for turn %s cannot cover the correction (has $%.6f, needs $%.6f)",
			parentTurn, parent.recorded.Float64, debit)
	}

	negated := modelinfo.Tokens{}
	for class, n := range from {
		negated[class] = -n
	}
	if err := tx.addTokens(parent.id, negated); err != nil {
		return "", err
	}
	if _, err := tx.tx.Exec(`UPDATE api_calls SET legacy_calculated_cost_usd = legacy_calculated_cost_usd - ? WHERE id = ?`,
		debit, parent.id); err != nil {
		return "", fmt.Errorf("debit parent call: %w", err)
	}
	if err := tx.addTokens(sub.id, modelinfo.Tokens{
		modelinfo.ClassInput: c.Counts.Input, modelinfo.ClassOutput: c.Counts.Output,
		modelinfo.ClassCacheRead: c.Counts.CacheRead, modelinfo.ClassCacheWrite5m: c.Counts.CacheWrite,
	}); err != nil {
		return "", err
	}
	if _, err := tx.tx.Exec(`UPDATE api_calls SET legacy_calculated_cost_usd = COALESCE(legacy_calculated_cost_usd, 0) + ? WHERE id = ?`,
		c.CostUSD, sub.id); err != nil {
		return "", fmt.Errorf("credit subagent call: %w", err)
	}
	return parentTurn, nil
}

// legacyTarget is one side of a correction.
type legacyTarget struct {
	id         int64
	tokens     modelinfo.Tokens
	recorded   sql.NullFloat64
	turnTotals bool
}

// The two sides of a correction: a turn's legacy parent call, and one
// subagent's legacy share (turn, subagent, model).
const (
	legacyParentQuery = `SELECT id, legacy_calculated_cost_usd,
			COALESCE(json_extract(detail, '$.turn_totals'), 0)
		FROM api_calls WHERE kind = 'legacy' AND turn_id = ? AND actor = ''
		  AND json_extract(detail, '$.v1_call_type') = 'delegated_turn'`
	legacyShareQuery = `SELECT id, legacy_calculated_cost_usd,
			COALESCE(json_extract(detail, '$.turn_totals'), 0)
		FROM api_calls WHERE kind = 'legacy' AND turn_id = ? AND actor = ? AND model = ?
		  AND json_extract(detail, '$.v1_call_type') = 'subagent_turn'`
)

// oneLegacyCall finds the single legacy call query matches. Nothing in the
// schema makes these keys unique, so the match count is the only thing
// standing between a correction and rewriting an unrelated call.
func (tx *Tx) oneLegacyCall(query, what string, args ...any) (legacyTarget, error) {
	rows, err := tx.tx.Query(query, args...)
	if err != nil {
		return legacyTarget{}, fmt.Errorf("find %s: %w", what, err)
	}
	var found []legacyTarget
	for rows.Next() {
		var t legacyTarget
		if err := rows.Scan(&t.id, &t.recorded, &t.turnTotals); err != nil {
			_ = rows.Close()
			return legacyTarget{}, fmt.Errorf("scan %s: %w", what, err)
		}
		found = append(found, t)
	}
	_ = rows.Close()
	if len(found) != 1 {
		return legacyTarget{}, fmt.Errorf("%s matched %d calls, want exactly 1", what, len(found))
	}
	t := found[0]
	if t.tokens, err = tx.tokens(t.id); err != nil {
		return legacyTarget{}, err
	}
	return t, nil
}
