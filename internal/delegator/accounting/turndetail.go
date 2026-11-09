package accounting

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"foci/internal/modelinfo"
)

// TurnDetail is one turn's facts and spend, for the app's "Message info"
// (#2992). Every figure comes from the ledger's tables and views: cost from
// turn_costs (call_costs underneath), never a stored column.
type TurnDetail struct {
	TurnID, Session, AgentID, Backend string
	Source, Purpose                   string
	StopReason, FinalModel            string
	StartedAt                         time.Time
	EndedAt                           time.Time // zero: the turn has not ended
	// Running: some spend attributable to the turn may still be coming (a
	// background subagent), turn_costs.still_running.
	Running bool

	// Tokens are the session thread's own calls (actor ''), by class;
	// SubagentTokens are its subagents'. Counts-basis calls only: a legacy
	// recorded figure has no class split (turn_class_costs).
	Tokens, SubagentTokens modelinfo.Tokens
	// Calls counts the thread's own model calls (kind 'call');
	// SubagentCalls its subagents' calls; Compactions the compaction calls
	// the turn caused: booked on the turn itself, or on the compaction turn
	// named after it ("<turn>:compaction", CompactionCostUSD's source, whose
	// spend is not in CostUSD).
	Calls, SubagentCalls, Compactions int
	CompactionCostUSD                 float64

	// CostUSD sums the priced calls, SubagentCostUSD the subagents' share of
	// it. UnpricedCalls counts the calls with no price: CostUSD leaves them
	// out, so a non-zero count means the cost is a lower bound.
	CostUSD, SubagentCostUSD float64
	UnpricedCalls            int
	// ContextFill is the in-context tokens of the thread's last call: the
	// context's size after the turn, as /context derives it. 0 = unknown.
	ContextFill int
}

// TurnDetail reads one turn. A turn the ledger does not hold is (nil, nil).
func (l *Ledger) TurnDetail(turnID string) (*TurnDetail, error) {
	if l == nil || turnID == "" {
		return nil, nil
	}
	d := TurnDetail{TurnID: turnID}
	var agent, purpose, stop, model, ended sql.NullString
	var started string
	var running sql.NullBool
	var fill sql.NullInt64
	err := l.db.QueryRow(`SELECT session, agent_id, backend, source, purpose,
			started_at, ended_at, stop_reason, final_model, cost_usd,
			subagent_cost_usd, unpriced_calls, still_running, context_fill
		FROM turn_costs WHERE turn_id = ?`, turnID).
		Scan(&d.Session, &agent, &d.Backend, &d.Source, &purpose, &started, &ended,
			&stop, &model, &d.CostUSD, &d.SubagentCostUSD, nullInt{&d.UnpricedCalls}, &running, &fill)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: turn %q: %w", turnID, err)
	}
	d.AgentID, d.Purpose, d.StopReason, d.FinalModel = agent.String, purpose.String, stop.String, model.String
	d.Running, d.ContextFill = running.Bool, int(fill.Int64)
	if d.StartedAt, err = time.Parse(timeLayout, started); err != nil {
		return nil, fmt.Errorf("ledger: turn %q started_at %q: %w", turnID, started, err)
	}
	if ended.Valid && ended.String != "" {
		if d.EndedAt, err = time.Parse(timeLayout, ended.String); err != nil {
			return nil, fmt.Errorf("ledger: turn %q ended_at %q: %w", turnID, ended.String, err)
		}
	}

	compactionTurn := turnID + ":compaction"
	if err := l.db.QueryRow(`SELECT
			SUM(turn_id = ? AND actor = '' AND kind = 'call'),
			SUM(turn_id = ? AND actor <> '' AND kind IN ('call', 'legacy')),
			SUM(kind = 'compaction'),
			TOTAL(CASE WHEN turn_id = ? THEN cost_usd END)
		FROM call_costs WHERE turn_id IN (?, ?)`, turnID, turnID, compactionTurn, turnID, compactionTurn).
		Scan(nullInt{&d.Calls}, nullInt{&d.SubagentCalls}, nullInt{&d.Compactions}, &d.CompactionCostUSD); err != nil {
		return nil, fmt.Errorf("ledger: turn %q calls: %w", turnID, err)
	}

	rows, err := l.db.Query(`SELECT actor <> '', class, SUM(count) FROM turn_class_costs
		WHERE turn_id = ? GROUP BY actor <> '', class`, turnID)
	if err != nil {
		return nil, fmt.Errorf("ledger: turn %q tokens: %w", turnID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var sub bool
		var class string
		var n int
		if err := rows.Scan(&sub, &class, &n); err != nil {
			return nil, fmt.Errorf("ledger: turn %q tokens: %w", turnID, err)
		}
		dst := &d.Tokens
		if sub {
			dst = &d.SubagentTokens
		}
		if *dst == nil {
			*dst = modelinfo.Tokens{}
		}
		(*dst)[modelinfo.Class(class)] += n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: turn %q tokens: %w", turnID, err)
	}
	return &d, nil
}

// SpawnLink records a spawn against the turn that started it (#2992), so the
// app's Message info can list a turn's spawns from a record, not a guess.
// Exactly where its spend is booked depends on how it ran: ChildSession for a
// spawn with a session of its own (a delegated batch, a background branch),
// ChildTurn for an API one-shot booked on a turn of its own, InParentTurn for
// one booked on the parent turn itself (its spend is already in that turn's
// cost). None of the three: its spend is not attributable.
type SpawnLink struct {
	SpawnID, ParentTurnID, ParentSession string
	Name, Mode                           string
	StartedAt                            time.Time
}

// RecordSpawn stores a spawn's link to its parent turn. A spawn with no parent
// turn is not stored: there is nothing to list it under.
func (l *Ledger) RecordSpawn(s SpawnLink) error {
	if l == nil || s.SpawnID == "" || s.ParentTurnID == "" {
		return nil
	}
	_, err := l.db.Exec(`INSERT OR IGNORE INTO spawn_links
		(spawn_id, parent_turn_id, parent_session, name, mode, started_at) VALUES (?, ?, ?, ?, ?, ?)`,
		s.SpawnID, s.ParentTurnID, s.ParentSession, nullIfEmpty(s.Name), nullIfEmpty(s.Mode), formatTime(s.StartedAt))
	if err != nil {
		return fmt.Errorf("ledger: record spawn %q: %w", s.SpawnID, err)
	}
	return nil
}

// SetSpawnBooking records where spawn spawnID's spend is booked, once known:
// its own session, its own turn, or the parent turn (inParent). Empty
// arguments leave the stored value alone.
func (l *Ledger) SetSpawnBooking(spawnID, childSession, childTurn string, inParent bool) error {
	if l == nil || spawnID == "" {
		return nil
	}
	_, err := l.db.Exec(`UPDATE spawn_links SET
			child_session = COALESCE(?, child_session),
			child_turn    = COALESCE(?, child_turn),
			in_parent_turn = MAX(in_parent_turn, ?)
		WHERE spawn_id = ?`, nullIfEmpty(childSession), nullIfEmpty(childTurn), inParent, spawnID)
	if err != nil {
		return fmt.Errorf("ledger: spawn %q booking: %w", spawnID, err)
	}
	return nil
}

// SpawnSpend is one recorded spawn of a turn with its spend so far.
type SpawnSpend struct {
	SpawnID, Name, Mode string
	StartedAt           time.Time
	// ChildSession is the spawn's own session, "" when it has none.
	ChildSession string
	// Priced: CostUSD and UnpricedCalls come from the spawn's own session or
	// turn. InParentTurn: its spend is part of the parent turn's cost.
	// Neither: where its spend went was not recorded.
	Priced, InParentTurn bool
	CostUSD              float64
	UnpricedCalls        int
}

// TurnSpawns lists the spawns recorded against turnID, oldest first.
func (l *Ledger) TurnSpawns(turnID string) ([]SpawnSpend, error) {
	if l == nil || turnID == "" {
		return nil, nil
	}
	rows, err := l.db.Query(`SELECT spawn_id, COALESCE(name, ''), COALESCE(mode, ''), started_at,
			COALESCE(child_session, ''), COALESCE(child_turn, ''), in_parent_turn
		FROM spawn_links WHERE parent_turn_id = ? ORDER BY started_at, spawn_id`, turnID)
	if err != nil {
		return nil, fmt.Errorf("ledger: spawns of %q: %w", turnID, err)
	}
	type row struct {
		s         SpawnSpend
		childTurn string
		inParent  bool
	}
	var rs []row
	for rows.Next() {
		var r row
		var started string
		if err := rows.Scan(&r.s.SpawnID, &r.s.Name, &r.s.Mode, &started, &r.s.ChildSession, &r.childTurn, &r.inParent); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("ledger: spawns of %q: %w", turnID, err)
		}
		if r.s.StartedAt, err = time.Parse(timeLayout, started); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("ledger: spawn %q started_at %q: %w", r.s.SpawnID, started, err)
		}
		rs = append(rs, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("ledger: spawns of %q: %w", turnID, err)
	}
	_ = rows.Close()
	out := make([]SpawnSpend, 0, len(rs))
	for _, r := range rs {
		s := r.s
		var q, arg string
		switch {
		case s.ChildSession != "":
			q, arg = `SELECT TOTAL(cost_usd), SUM(cost_usd IS NULL) FROM call_costs WHERE session = ?`, s.ChildSession
		case r.childTurn != "":
			q, arg = `SELECT TOTAL(cost_usd), SUM(cost_usd IS NULL) FROM call_costs WHERE turn_id = ?`, r.childTurn
		}
		if q != "" {
			if err := l.db.QueryRow(q, arg).Scan(&s.CostUSD, nullInt{&s.UnpricedCalls}); err != nil {
				return nil, fmt.Errorf("ledger: spawn %q spend: %w", s.SpawnID, err)
			}
			s.Priced = true
		} else {
			s.InParentTurn = r.inParent
		}
		out = append(out, s)
	}
	return out, nil
}
