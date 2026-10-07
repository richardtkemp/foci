package accounting

import (
	"database/sql"
	"fmt"
	"time"

	"foci/internal/modelinfo"
)

// CallRow is one call as the cost views price it: what /cost, /cache, /last
// and /status read, from api.db or, with none, from api.jsonl (ReadJSONL).
type CallRow struct {
	ID                       int64
	BilledAt                 time.Time
	Backend, Provider, Model string
	Session, AgentID, TurnID string
	// Actor is "" for the session's own thread, else the subagent.
	Actor           string
	Kind, CostBasis string
	// CostUSD is call_costs.cost_usd: nil when a billed class is unpriced.
	CostUSD *float64
	// Classes is the call's counts and, on the counts basis, each class's
	// cost (call_class_costs). A recorded-basis call keeps its counts for
	// forensics, but they carry no cost: its figure was never split by class.
	Classes map[modelinfo.Class]ClassCost
	// Fill is the context the call left (Booking.Fill).
	Fill    int
	Purpose string
}

// ClassCost is one class of one call: its count and, where priced, its cost.
type ClassCost struct {
	Count   int
	CostUSD *float64
}

// Cost is the call's cost, 0 when unpriced; Priced says which.
func (r CallRow) Cost() float64 {
	if r.CostUSD == nil {
		return 0
	}
	return *r.CostUSD
}

// Priced reports whether the views could price the call.
func (r CallRow) Priced() bool { return r.CostUSD != nil }

// Subagent reports whether a subagent, not the session's own thread, made
// the call.
func (r CallRow) Subagent() bool { return r.Actor != "" }

// Counted reports whether the row is a call of its own. A legacy subagent
// share is not: a pre-ledger turn that spawned three subagents was one row
// per subagent beside its parent row, and counting four would inflate every
// "N calls" figure (#1880 phase C). Its cost still counts.
func (r CallRow) Counted() bool { return !(r.Kind == KindLegacy && r.Actor != "") }

// Count sums the call's counts over classes.
func (r CallRow) Count(classes ...modelinfo.Class) int {
	n := 0
	for _, c := range classes {
		n += r.Classes[c].Count
	}
	return n
}

// Calls reads every call billed at or after since, in billing order.
func (l *Ledger) Calls(since time.Time) ([]CallRow, error) {
	rows, err := l.db.Query(`SELECT c.id, c.billed_at, c.backend, c.provider, c.model, c.session,
			COALESCE(c.agent_id, ''), COALESCE(c.turn_id, ''), c.actor, c.kind, c.cost_basis, cc.cost_usd,
			COALESCE(t.purpose, ''),
			COALESCE(t.legacy_input, 0) + COALESCE(t.legacy_cache_read, 0) + COALESCE(t.legacy_cache_write, 0)
		FROM api_calls c
		JOIN call_costs cc ON cc.id = c.id
		LEFT JOIN turns t ON t.turn_id = c.turn_id
		WHERE c.billed_at >= ?
		ORDER BY c.billed_at, c.id`, formatTime(since))
	if err != nil {
		return nil, fmt.Errorf("ledger: read calls: %w", err)
	}
	var out []CallRow
	index := map[int64]int{}
	for rows.Next() {
		var r CallRow
		var billed string
		var cost sql.NullFloat64
		var legacyFill int
		if err := rows.Scan(&r.ID, &billed, &r.Backend, &r.Provider, &r.Model, &r.Session,
			&r.AgentID, &r.TurnID, &r.Actor, &r.Kind, &r.CostBasis, &cost, &r.Purpose, &legacyFill); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("ledger: scan call: %w", err)
		}
		if r.BilledAt, err = time.Parse(timeLayout, billed); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("ledger: call %d billed_at %q: %w", r.ID, billed, err)
		}
		if cost.Valid {
			v := cost.Float64
			r.CostUSD = &v
		}
		// A legacy call's counts are turn totals, not a context size: its
		// parent row's fill is the copy on its turn (R4).
		if r.Kind == KindLegacy && r.Actor == "" {
			r.Fill = legacyFill
		}
		r.Classes = map[modelinfo.Class]ClassCost{}
		index[r.ID] = len(out)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("ledger: read calls: %w", err)
	}
	_ = rows.Close()

	cls, err := l.db.Query(`SELECT x.call_id, x.class, x.count, x.cost_usd
		FROM call_class_costs x JOIN api_calls c ON c.id = x.call_id
		WHERE c.billed_at >= ?`, formatTime(since))
	if err != nil {
		return nil, fmt.Errorf("ledger: read call classes: %w", err)
	}
	defer func() { _ = cls.Close() }()
	for cls.Next() {
		var id int64
		var class string
		var n int
		var cost sql.NullFloat64
		if err := cls.Scan(&id, &class, &n, &cost); err != nil {
			return nil, fmt.Errorf("ledger: scan call class: %w", err)
		}
		i, ok := index[id]
		if !ok {
			continue // booked between the two reads
		}
		r := &out[i]
		cc := ClassCost{Count: n}
		if cost.Valid && r.CostBasis != CostBasisRecorded {
			v := cost.Float64
			cc.CostUSD = &v
		}
		r.Classes[modelinfo.Class(class)] = cc
		if r.Kind != KindLegacy && modelinfo.InContext(modelinfo.Class(class)) {
			r.Fill += n
		}
	}
	return out, cls.Err()
}

// SessionStats is one session's totals, for /status and /context.
type SessionStats struct {
	// TurnCount counts the session's own turns: not compactions, nor the
	// system turns a helper call (summary, spawn) is booked on.
	TurnCount int
	// TotalCalls counts calls (CallRow.Counted); TotalCost sums the priced
	// ones and UnpricedCalls counts the rest.
	TotalCalls    int
	TotalCost     float64
	UnpricedCalls int
	CreatedAt     time.Time
	LastActivity  time.Time
	// ContextTokens is the context fill of the session's latest turn that
	// had any (turn_costs.context_fill).
	ContextTokens int
}

// SessionStats reads one session's totals.
func (l *Ledger) SessionStats(session string) (*SessionStats, error) {
	var st SessionStats
	var first, last sql.NullString
	if err := l.db.QueryRow(`SELECT
			COUNT(*) - SUM(kind = 'legacy' AND actor <> ''),
			TOTAL(cost_usd), SUM(cost_usd IS NULL), MIN(billed_at), MAX(billed_at)
		FROM call_costs WHERE session = ?`, session).
		Scan(nullInt{&st.TotalCalls}, &st.TotalCost, nullInt{&st.UnpricedCalls}, &first, &last); err != nil {
		return nil, fmt.Errorf("ledger: session stats: %w", err)
	}
	st.CreatedAt, _ = time.Parse(timeLayout, first.String)
	st.LastActivity, _ = time.Parse(timeLayout, last.String)
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM turns
		WHERE session = ? AND source NOT IN ('compaction', 'system')`, session).Scan(&st.TurnCount); err != nil {
		return nil, fmt.Errorf("ledger: session turns: %w", err)
	}
	// Synthetic turns (no inference, model "<synthetic>") leave no fill;
	// skipping them keeps one from hiding the real size of the session.
	var fill sql.NullInt64
	if err := l.db.QueryRow(`SELECT context_fill FROM turn_costs
		WHERE session = ? AND source NOT IN ('compaction', 'system') AND context_fill > 0
		ORDER BY started_at DESC LIMIT 1`, session).Scan(&fill); err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("ledger: session context: %w", err)
	}
	st.ContextTokens = int(fill.Int64)
	return &st, nil
}

// CurrentContextFill is the session's context fill as SessionStats reports
// it, but only while it still describes the context: 0 (unknown) when a
// compaction is booked after the latest fill. A compaction turn leaves no
// fill of its own, so SessionStats keeps reporting the pre-compaction size
// until the next real turn; a caller that ACTS on the size (quiet-hours
// compaction, #2218/#2235) must not read that stale figure as current.
func (l *Ledger) CurrentContextFill(session string) (int, error) {
	var fill sql.NullInt64
	err := l.db.QueryRow(`SELECT f.context_fill FROM turn_costs f
		WHERE f.session = ? AND f.source NOT IN ('compaction', 'system') AND f.context_fill > 0
		  AND NOT EXISTS (SELECT 1 FROM turn_costs c
			WHERE c.session = f.session AND c.source = 'compaction' AND c.started_at > f.started_at)
		ORDER BY f.started_at DESC LIMIT 1`, session).Scan(&fill)
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("ledger: current context fill: %w", err)
	}
	return int(fill.Int64), nil
}

// LastTurnID is the session's newest conversation turn, or "": the durable
// counterpart of telemetry.LastTurnID for a session whose last turn predates
// this process. A turn the migration minted for a pre-#1695 row that named
// none ("legacy:<id>") was never traced, so it is not an answer.
func (l *Ledger) LastTurnID(session string) (string, error) {
	var id string
	err := l.db.QueryRow(`SELECT turn_id FROM turns
		WHERE session = ? AND source NOT IN ('compaction', 'system') AND turn_id NOT LIKE 'legacy:%'
		ORDER BY started_at DESC LIMIT 1`, session).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("ledger: last turn: %w", err)
	}
	return id, nil
}

// nullInt scans a nullable integer aggregate (SUM over no rows is NULL) as 0.
type nullInt struct{ p *int }

func (n nullInt) Scan(v any) error {
	var x sql.NullInt64
	if err := x.Scan(v); err != nil {
		return err
	}
	*n.p = int(x.Int64)
	return nil
}
