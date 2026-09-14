package log

import (
	"database/sql"
	"fmt"
	"strings"

	"foci/internal/modelinfo"
	"foci/internal/timeutil"
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
		if c.SubagentTurnID == "" || c.AgentID == "" || c.BilledAt.IsZero() {
			continue
		}
		parentTurn, err := applyOneCorrection(apiLog.db, c)
		if err != nil {
			std.event(WARN, "api_db", "cost correction skipped (billed %s -> subagent %s on turn %s, $%.6f): %v",
				timeutil.Format(c.BilledAt), c.AgentID, c.SubagentTurnID, c.CostUSD, err)
			continue
		}
		std.event(INFO, "api_db", "cost correction applied: $%.6f (%d cache-read, %d cache-write) "+
			"moved from parent turn %s to subagent %s on turn %s (#1918)",
			c.CostUSD, c.Counts.CacheRead, c.Counts.CacheWrite,
			parentTurn, c.AgentID, c.SubagentTurnID)
	}
}

// applyOneCorrection performs one correction atomically, returning the parent
// turn it debited, or an error naming which half failed.
func applyOneCorrection(db *sql.DB, c modelinfo.CostCorrection) (parentTurn string, err error) {
	tx, err := db.Begin()
	if err != nil {
		return "", fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// WHICH parent row absorbed the spend, resolved from the billing time.
	//
	// A turn is priced from the PREVIOUS result, so its window runs from the
	// previous turn's close to its own: the timeline tiles with no gaps, and
	// idle time belongs to the turn that FOLLOWS it. The turn that absorbed
	// spend billed at t is therefore the first of this session to CLOSE at or
	// after t.
	//
	// CLOSE IS ts + duration_ms, NOT ts. api_calls.ts is the turn's START —
	// turn_delegated.go passes ts.StartedAt, and the turn_id nanos match ts
	// exactly. The first version of this compared against ts alone, which means
	// "first turn to START at or after t", so spend billed while a turn was
	// RUNNING skipped that turn and debited the NEXT one, which never absorbed
	// it. Since every subagent bills while the turn that spawned it runs, that
	// was the entire target population (#1922).
	//
	// The close is verified against the log's own turn_lifecycle
	// event=complete lines: start 15:41:35 + 15,648ms = 15:41:50, logged
	// 15:41:50. A NULL duration_ms degrades to start-based rather than
	// excluding the row.
	//
	// A "verification" of the old form appeared to pass because its example sat
	// in a 25-minute IDLE GAP — the one case where start-based and close-based
	// resolution agree.
	//
	// The session is the part of the turn id before '@' — the format is
	// "<session>@<StartedAt UnixNano>".
	//
	// unixepoch() on BOTH sides, never a string compare: ts is stored as local
	// ISO WITH OFFSET, and SQLite reads a bare comparison lexically while
	// date() silently converts to UTC. That mismatch cost 6.8% on the morning
	// cost report (#1896).
	session, _, ok := strings.Cut(c.SubagentTurnID, "@")
	if !ok || session == "" {
		return "", fmt.Errorf("subagent turn id %q has no session prefix", c.SubagentTurnID)
	}
	if err = tx.QueryRow(`SELECT turn_id FROM api_calls
		WHERE session = ? AND call_type = 'delegated_turn' AND turn_id IS NOT NULL AND turn_id <> ''
		  AND unixepoch(ts, 'utc') + COALESCE(duration_ms, 0) / 1000.0 >= unixepoch(?, 'utc')
		ORDER BY unixepoch(ts, 'utc') ASC LIMIT 1`,
		session, timeutil.Format(c.BilledAt)).Scan(&parentTurn); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("no turn of session %s closed at or after %s",
				session, timeutil.Format(c.BilledAt))
		}
		return "", fmt.Errorf("resolve parent turn: %w", err)
	}

	// The parent row must be able to give up the amount. Read it first rather
	// than subtracting and inspecting the result: a row that cannot cover the
	// correction means the correction is wrong, and finding that out BEFORE
	// writing keeps the failure a warning instead of a repair.
	var in, out, cr, cw sql.NullInt64
	var cost sql.NullFloat64
	row := tx.QueryRow(`SELECT turn_input_tokens, turn_output_tokens, turn_cache_read_tokens,
		turn_cache_write_tokens, calculated_cost_usd
		FROM api_calls WHERE turn_id = ? AND call_type = 'delegated_turn'`, parentTurn)
	if err = row.Scan(&in, &out, &cr, &cw, &cost); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("no parent row for turn %s", parentTurn)
		}
		return "", fmt.Errorf("read parent row: %w", err)
	}
	// A NULL count is "never measured", not zero, and subtracting from it would
	// assert a measurement that was never made.
	if !in.Valid || !out.Valid || !cr.Valid || !cw.Valid || !cost.Valid {
		return "", fmt.Errorf("parent row for turn %s has unmeasured columns", parentTurn)
	}
	if in.Int64 < int64(c.Counts.Input) || out.Int64 < int64(c.Counts.Output) ||
		cr.Int64 < int64(c.Counts.CacheRead) || cw.Int64 < int64(c.Counts.CacheWrite) ||
		cost.Float64 < c.CostUSD {
		return "", fmt.Errorf("parent row for turn %s cannot cover the correction "+
			"(has in=%d out=%d cr=%d cw=%d $%.6f, needs in=%d out=%d cr=%d cw=%d $%.6f)",
			parentTurn, in.Int64, out.Int64, cr.Int64, cw.Int64, cost.Float64,
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
		c.CostUSD, parentTurn)
	if err != nil {
		return "", fmt.Errorf("update parent row: %w", err)
	}
	if err = exactlyOne(res, "parent row for turn "+parentTurn); err != nil {
		return "", err
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
		return "", fmt.Errorf("update subagent row: %w", err)
	}
	if err = exactlyOne(res, "subagent row for turn "+c.SubagentTurnID+" agent "+c.AgentID); err != nil {
		return "", err
	}

	if err = tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return parentTurn, nil
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

// AccumulateSubagentRow folds one subagent's spend into THE single row for
// (turn_id, agent_id, model), creating it on first sight (#1922).
//
// Phase C used to INSERT unconditionally, so a subagent that outlived its parent
// got one row per turn it straddled — all under the SPAWNING turn id, because
// that is the attribution. Live measurement found keys with 2 and 3 rows. Two
// consequences, both bad:
//
//   - "What did that delegation cost?" needed a SUM across rows, when the whole
//     point of phase C was to make it a lookup.
//   - The #1918 correction targets a row by (turn_id, agent_id, model) and
//     requires exactly one match, so it could NEVER apply to a background
//     subagent — precisely the population it exists for.
//
// Accumulating instead of inserting makes the key unique for everything written
// from here on, so both problems go away at the source rather than being worked
// around at the reader. Rows written before this change keep their duplicates;
// a correction against one of those still refuses, safely and loudly.
//
// Returns true if an existing row was extended, false if a new one was written.
func AccumulateSubagentRow(entry APIEntry) bool {
	if apiLog == nil || apiLog.db == nil || entry.TurnID == "" || entry.AgentID == "" {
		API(entry)
		return false
	}
	apiLog.mu.Lock()
	res, err := apiLog.db.Exec(`UPDATE api_calls SET
			turn_input_tokens       = COALESCE(turn_input_tokens, 0)       + ?,
			turn_output_tokens      = COALESCE(turn_output_tokens, 0)      + ?,
			turn_cache_read_tokens  = COALESCE(turn_cache_read_tokens, 0)  + ?,
			turn_cache_write_tokens = COALESCE(turn_cache_write_tokens, 0) + ?,
			output_tokens           = COALESCE(output_tokens, 0)           + ?,
			calculated_cost_usd     = COALESCE(calculated_cost_usd, 0)     + ?,
			duration_ms             = ?
		WHERE turn_id = ? AND call_type = 'subagent_turn' AND agent_id = ? AND model = ?
		  AND id = (SELECT MIN(id) FROM api_calls
		            WHERE turn_id = ? AND call_type = 'subagent_turn' AND agent_id = ? AND model = ?)`,
		counts(entry).Input, counts(entry).Output, counts(entry).CacheRead, counts(entry).CacheWrite,
		counts(entry).Output, effectiveCalculated(entry), entry.DurationMS,
		entry.TurnID, entry.AgentID, entry.Model,
		entry.TurnID, entry.AgentID, entry.Model)
	apiLog.mu.Unlock()
	if err == nil {
		if n, _ := res.RowsAffected(); n == 1 {
			// The db row is collapsed; the JSONL still gets this instalment as
			// its own line. Every JSONL reader sums rows, so the file stays a
			// correct (if un-collapsed) record — whereas dropping the line
			// would make the fallback under-report the delegation.
			APIJSONLOnly(entry)
			return true
		}
	}
	// No existing row (or the update failed): write the first one. The JSONL
	// gets this insert either way, which is why it is only ever a fallback
	// source — it is reset on every restart.
	API(entry)
	return false
}

// counts returns the entry's turn-summed counts, or the zero value.
func counts(e APIEntry) modelinfo.TokenCounts {
	if e.Turn == nil {
		return modelinfo.TokenCounts{}
	}
	return *e.Turn
}

// effectiveCalculated returns the entry's own priced figure, or zero.
func effectiveCalculated(e APIEntry) float64 {
	if e.CalculatedCostUSD == nil {
		return 0
	}
	return *e.CalculatedCostUSD
}
