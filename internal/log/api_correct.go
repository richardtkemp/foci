package log

import (
	"database/sql"
	"fmt"

	"foci/internal/modelinfo"
)

// ApplyCostCorrections moves late-arriving subagent spend off the parent row
// that absorbed it and onto the subagent row that should have carried it
// (#1918, following #1909).
//
// WHY AN UPDATE AND NOT A THIRD ROW. A subagent's spend reaches foci by tailing
// its transcript, which lags billing by anything from a poll interval to the
// 60s the tailer will wait for the file to appear. Spend still undelivered when
// a turn's result closes is nonetheless inside that turn's authoritative
// ModelUsage, so the parent share — ModelUsage minus what had been delivered —
// absorbs it. The turn total is right and its split is not. Appending a signed
// correction row would fix the arithmetic while making every reader sum rows to
// learn the truth; the requirement is that a plain lookup is already correct
// (Dick, 2026-09-14: "I don't want a correcting pair, I just want a single
// correct entry").
//
// ALL OR NOTHING, PER CORRECTION. Each correction is one transaction, and both
// sides must match exactly one row or the whole thing rolls back. A half-applied
// correction adds without subtracting, which inflates the record — the precise
// failure #1909 existed to close, and not one worth reintroducing to salvage an
// attribution. A skip costs only the attribution, which is the status quo.
//
// The parent side is also refused if it would go NEGATIVE. A negative token
// count prices as a credit and would quietly reduce the bill; it also means the
// model behind the correction is wrong, which is worth a warning rather than a
// silent clamp.
func ApplyCostCorrections(cs []modelinfo.CostCorrection) {
	if apiLog == nil || apiLog.db == nil || len(cs) == 0 {
		return
	}
	apiLog.mu.Lock()
	defer apiLog.mu.Unlock()

	for _, c := range cs {
		if c.ParentTurnID == "" || c.SubagentTurnID == "" || c.AgentID == "" {
			continue
		}
		if err := applyOneCorrection(apiLog.db, c); err != nil {
			std.event(WARN, "api_db", "cost correction skipped (%s -> %s, agent=%s, $%.6f): %v",
				c.ParentTurnID, c.SubagentTurnID, c.AgentID, c.CostUSD, err)
			continue
		}
		std.event(INFO, "api_db", "cost correction applied: $%.6f (%d cache-read, %d cache-write) "+
			"moved from parent turn %s to subagent %s on turn %s (#1918)",
			c.CostUSD, c.Counts.CacheRead, c.Counts.CacheWrite,
			c.ParentTurnID, c.AgentID, c.SubagentTurnID)
	}
}

// applyOneCorrection performs one correction atomically, returning an error
// that names which half failed — the caller logs it and moves on.
func applyOneCorrection(db *sql.DB, c modelinfo.CostCorrection) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// The parent row must be able to give up the amount. Read it first rather
	// than subtracting and inspecting the result: a row that cannot cover the
	// correction means the correction is wrong, and finding that out BEFORE
	// writing keeps the failure a warning instead of a repair.
	var in, out, cr, cw sql.NullInt64
	var cost sql.NullFloat64
	row := tx.QueryRow(`SELECT turn_input_tokens, turn_output_tokens, turn_cache_read_tokens,
		turn_cache_write_tokens, calculated_cost_usd
		FROM api_calls WHERE turn_id = ? AND call_type = 'delegated_turn'`, c.ParentTurnID)
	if err = row.Scan(&in, &out, &cr, &cw, &cost); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("no parent row for turn %s", c.ParentTurnID)
		}
		return fmt.Errorf("read parent row: %w", err)
	}
	// A NULL count is "never measured", not zero, and subtracting from it would
	// assert a measurement that was never made.
	if !in.Valid || !out.Valid || !cr.Valid || !cw.Valid || !cost.Valid {
		return fmt.Errorf("parent row for turn %s has unmeasured columns", c.ParentTurnID)
	}
	if in.Int64 < int64(c.Counts.Input) || out.Int64 < int64(c.Counts.Output) ||
		cr.Int64 < int64(c.Counts.CacheRead) || cw.Int64 < int64(c.Counts.CacheWrite) ||
		cost.Float64 < c.CostUSD {
		return fmt.Errorf("parent row for turn %s cannot cover the correction "+
			"(has in=%d out=%d cr=%d cw=%d $%.6f, needs in=%d out=%d cr=%d cw=%d $%.6f)",
			c.ParentTurnID, in.Int64, out.Int64, cr.Int64, cw.Int64, cost.Float64,
			c.Counts.Input, c.Counts.Output, c.Counts.CacheRead, c.Counts.CacheWrite, c.CostUSD)
	}

	res, err := tx.Exec(`UPDATE api_calls SET
			turn_input_tokens       = turn_input_tokens - ?,
			turn_output_tokens      = turn_output_tokens - ?,
			turn_cache_read_tokens  = turn_cache_read_tokens - ?,
			turn_cache_write_tokens = turn_cache_write_tokens - ?,
			calculated_cost_usd     = calculated_cost_usd - ?
		WHERE turn_id = ? AND call_type = 'delegated_turn'`,
		c.Counts.Input, c.Counts.Output, c.Counts.CacheRead, c.Counts.CacheWrite,
		c.CostUSD, c.ParentTurnID)
	if err != nil {
		return fmt.Errorf("update parent row: %w", err)
	}
	if err = exactlyOne(res, "parent row for turn "+c.ParentTurnID); err != nil {
		return err
	}

	// output_tokens moves with turn_output_tokens on a SUBAGENT row only: there
	// the two are written from the same figure (turn_delegated.go sets
	// Output: counts.Output beside Turn: &counts), so leaving one behind would
	// break that row's own re-pricing identity (#1854). On the parent row
	// output_tokens means something else and is left alone.
	res, err = tx.Exec(`UPDATE api_calls SET
			turn_input_tokens       = turn_input_tokens + ?,
			turn_output_tokens      = turn_output_tokens + ?,
			turn_cache_read_tokens  = turn_cache_read_tokens + ?,
			turn_cache_write_tokens = turn_cache_write_tokens + ?,
			output_tokens           = output_tokens + ?,
			calculated_cost_usd     = calculated_cost_usd + ?
		WHERE turn_id = ? AND call_type = 'subagent_turn' AND agent_id = ? AND model = ?`,
		c.Counts.Input, c.Counts.Output, c.Counts.CacheRead, c.Counts.CacheWrite,
		c.Counts.Output, c.CostUSD, c.SubagentTurnID, c.AgentID, c.Model)
	if err != nil {
		return fmt.Errorf("update subagent row: %w", err)
	}
	if err = exactlyOne(res, "subagent row for turn "+c.SubagentTurnID+" agent "+c.AgentID); err != nil {
		return err
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// exactlyOne rejects an UPDATE that matched no row or several. Neither target
// has a unique constraint, so the row count is the only thing standing between
// a correction and silently rewriting an unrelated row.
func exactlyOne(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for %s: %w", what, err)
	}
	if n != 1 {
		return fmt.Errorf("%s matched %d rows, want exactly 1", what, n)
	}
	return nil
}
