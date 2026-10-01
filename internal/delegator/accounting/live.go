package accounting

import (
	"fmt"
	"sync/atomic"
	"time"
)

// Booking is one call as the ledger's observers see it, once the transaction
// that booked it has committed: one api.jsonl line and, with tracing on, one
// Langfuse generation (BookedHook). Every call reaches the observers through
// Book and nowhere else, which is what keeps the two in step with api.db.
type Booking struct {
	Call
	ID int64
	// CostUSD is the call's cost as the call_costs view prices it: its counts
	// at the rates in effect when it was billed, or its recorded figure on the
	// recorded basis; nil when a billed class is unpriced.
	CostUSD *float64
	// Purpose is the owning turn's batch purpose (#1962), "" on other turns.
	Purpose string
	// TurnSource is the owning turn's source: a conversation turn (user,
	// autonomous, keepalive, batch) or a helper call's turn of its own
	// (compaction, system).
	TurnSource string
	// Fill is the context the call leaves: its own in-context classes, or for
	// a legacy parent row the fill copied onto its turn.
	Fill int
}

// BookedHook, when set, observes every committed booking. The trace exporter
// (internal/telemetry) hangs its cost generations here. Called synchronously
// after the commit, so it must be quick and must never book.
var BookedHook func(Booking)

// observe hands one committed booking to the observers.
func observe(b Booking) {
	appendJSONL(b)
	if BookedHook != nil {
		BookedHook(b)
	}
}

// live is the process's ledger: the api.db the gateway opened at startup.
var live atomic.Pointer[Ledger]

// shadow is the scratch ledger an adapter under verification books into
// (logging.api_shadow_db), or nil. No adapter books there since the Claude
// Code switch (#2115); the next one to be verified in shadow reads it with a
// Shadow() getter, which went with its last caller.
var shadow atomic.Pointer[Ledger]

// SetShadow makes l the shadow ledger; nil detaches it.
func SetShadow(l *Ledger) { shadow.Store(l) }

// SetLive makes l the ledger Record books into; nil detaches it.
func SetLive(l *Ledger) { live.Store(l) }

// Live is the process's ledger, or nil when no api.db is configured.
func Live() *Ledger { return live.Load() }

// Record books one call on turn t in the live ledger, upserting the turn
// first so the call's turn always exists. It is how a backend adapter with
// one call in hand books it (the direct API). With no live ledger (no api_db
// configured, or a unit test) the call is still validated, written to
// api.jsonl and handed to BookedHook, as the pre-ledger writer did.
func Record(t Turn, c Call) error {
	_, err := Live().RecordCall(t, c, nil)
	return err
}

// RecordCall books one call on turn t, with the backend's own report of it
// when it has one (opencode's per-message cost), in one transaction. The turn
// is upserted first, and an upsert never overwrites a stored field with an
// empty one, so an adapter that knows only a turn's id, session and backend
// can name it without clobbering what the agent layer recorded at turn start.
// On a nil ledger the call is only observed (see Record).
func (l *Ledger) RecordCall(t Turn, c Call, r *Report) (Booked, error) {
	if t.Session == "" {
		t.Session = NoSession
	}
	if l == nil {
		if err := c.validate(); err != nil {
			return Booked{}, err
		}
		observe(Booking{Call: c, CostUSD: c.cost(), Purpose: t.Purpose, TurnSource: t.Source, Fill: c.fill()})
		return Booked{}, nil
	}
	var booked Booked
	err := l.Update(func(tx *Tx) error {
		// Overhead has no turn (R6).
		if t.TurnID != "" {
			if err := tx.RecordTurn(t); err != nil {
				return err
			}
		}
		var err error
		if booked, err = tx.Book(c); err != nil {
			return err
		}
		// A replayed message is not a second report of it.
		if r != nil && !booked.Duplicate {
			return tx.Report(*r)
		}
		return nil
	})
	return booked, err
}

// TurnCost is a turn's cost as turn_costs prices it — the whole turn, every
// call booked on it so far — and how many of its calls are unpriced.
func (l *Ledger) TurnCost(turnID string) (usd float64, unpriced int, err error) {
	err = l.db.QueryRow(`SELECT COALESCE(SUM(cost_usd), 0), SUM(cost_usd IS NULL)
		FROM call_costs WHERE turn_id = ?`, turnID).Scan(&usd, nullInt{&unpriced})
	if err != nil {
		return 0, 0, fmt.Errorf("ledger: turn %q cost: %w", turnID, err)
	}
	return usd, unpriced, nil
}

// RecordTurn upserts one turn's facts in the live ledger, if there is one.
func RecordTurn(t Turn) error {
	l := Live()
	if l == nil {
		return nil
	}
	return l.Update(func(tx *Tx) error { return tx.RecordTurn(t) })
}

// SetTurnActivity sets when the last spend attributable to a stored turn
// stopped; zero reopens it (still running, R8). It is for a backend whose
// spend can outlive the turn (a Claude Code background subagent), which
// closes the turn's activity itself instead of at the turn's end. A turn not
// stored is left alone. Unlike RecordTurn it overwrites: a turn reopened by
// a reactivated subagent must read as running again.
func (l *Ledger) SetTurnActivity(turnID string, closedAt time.Time) error {
	if l == nil || turnID == "" {
		return nil
	}
	return l.Update(func(tx *Tx) error {
		if _, err := tx.tx.Exec(`UPDATE turns SET activity_closed_at = ? WHERE turn_id = ?`,
			nullTime(closedAt), turnID); err != nil {
			return fmt.Errorf("ledger: turn %q activity: %w", turnID, err)
		}
		return nil
	})
}

// CloseOrphanedTurns closes the activity of every turn still marked running
// that began before start: called once as the gateway starts, when no backend
// process of an earlier gateway survives to close them (a foci crash or a
// shutdown mid-turn). Each closes at its last booked call, else its end, else
// its start. Returns how many it closed.
func (l *Ledger) CloseOrphanedTurns(start time.Time) (int64, error) {
	var n int64
	err := l.Update(func(tx *Tx) error {
		res, err := tx.tx.Exec(`UPDATE turns SET activity_closed_at = COALESCE(
				(SELECT MAX(c.billed_at) FROM api_calls c WHERE c.turn_id = turns.turn_id),
				ended_at, started_at)
			WHERE activity_closed_at IS NULL AND started_at < ?`, formatTime(start))
		if err != nil {
			return fmt.Errorf("ledger: close orphaned turns: %w", err)
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// RenderRates re-renders token_rates and token_classes from modelinfo, for a
// live-applied [[modelinfo]] change: the views price from that copy, so it
// must never lag the Go table (#2111 §11.11).
func (l *Ledger) RenderRates() error {
	return l.Update(func(tx *Tx) error { return renderRates(tx.tx) })
}
