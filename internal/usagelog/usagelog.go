// Package usagelog is the usage history store (#3103, Dick 2026-11-13: "Save
// every usage figure we receive to a database").
//
// Every usage report foci receives from a backend that supports /usage is
// written here: the spawn reserve's scheduled reads and the reads a spawn
// starts (#2662), fresh /mana and /usage reads, Claude Code's
// rate_limit_event of every type, and codex's account/rateLimits/updated and
// its start-up account/rateLimits/read. Each report keeps when foci received
// it, the backend, the usage account, the agent it came through, its source,
// each window's figures as reported, and the full payload.
//
// Retention is forever (Dick 2026-11-14, P5): nothing here deletes a row.
//
// /mana cached answers from Current, which reads only this store: it never
// starts or waits on a backend read, and it works after a restart, before
// any new read, with each figure's true age.
package usagelog

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"foci/internal/delegator"
	"foci/internal/sqlite"
)

// Store is the SQLite usage history.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the usage history at path.
func Open(path string) (*Store, error) {
	db, err := sqlite.OpenInit(path,
		`CREATE TABLE IF NOT EXISTS usage_reports (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			received_at INTEGER NOT NULL, -- Unix nanoseconds
			backend     TEXT NOT NULL,
			account     TEXT NOT NULL,
			agent_id    TEXT NOT NULL,
			source      TEXT NOT NULL,
			payload     TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS usage_windows (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			report_id     INTEGER NOT NULL REFERENCES usage_reports(id),
			account       TEXT NOT NULL,
			received_at   INTEGER NOT NULL, -- Unix nanoseconds, the report's
			window        TEXT NOT NULL,
			length_sec    INTEGER NOT NULL DEFAULT 0, -- 0 = unknown
			used          REAL,           -- as reported, in used_unit; NULL = none
			used_unit     TEXT NOT NULL DEFAULT '',
			resets_at     INTEGER NOT NULL DEFAULT 0, -- Unix seconds; 0 = not reported
			status        TEXT NOT NULL DEFAULT '',
			limit_reached INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS usage_windows_account ON usage_windows(account, window, received_at)`,
		`CREATE INDEX IF NOT EXISTS usage_reports_account ON usage_reports(account, received_at)`,
	)
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Save writes one report and its windows in one transaction. The report must
// carry its received time, backend, account, agent and source.
func (s *Store) Save(r delegator.UsageReport) error {
	if r.Account == "" || r.Source == "" || r.ReceivedAt.IsZero() {
		return fmt.Errorf("usagelog: report without account, source or received time")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("usagelog: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	at := r.ReceivedAt.UnixNano()
	res, err := tx.Exec(`INSERT INTO usage_reports (received_at, backend, account, agent_id, source, payload) VALUES (?, ?, ?, ?, ?, ?)`,
		at, r.Backend, r.Account, r.AgentID, r.Source, string(r.Payload))
	if err != nil {
		return fmt.Errorf("usagelog: insert report: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("usagelog: report id: %w", err)
	}
	for _, w := range r.Windows {
		var used any
		if w.Used != nil {
			used = *w.Used
		}
		var resets int64
		if !w.ResetsAt.IsZero() {
			resets = w.ResetsAt.Unix()
		}
		if _, err := tx.Exec(`INSERT INTO usage_windows (report_id, account, received_at, window, length_sec, used, used_unit, resets_at, status, limit_reached) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, r.Account, at, w.Window, int64(w.Length/time.Second), used, w.UsedUnit, resets, w.Status, boolInt(w.LimitReached)); err != nil {
			return fmt.Errorf("usagelog: insert window: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("usagelog: commit: %w", err)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// sameWindowSlack is how far two reset times may differ and still name the
// same window: codex's resetsAt wobbles by a second between reports, and a
// get_usage reset (RFC 3339) and a rate_limit_event reset (Unix seconds) of
// one window need not agree to the second.
const sameWindowSlack = 5 * time.Minute

// Current is the newest stored state of one usage account (/mana cached).
type Current struct {
	Account string
	Backend string // the newest report's backend; "" when none is stored
	Windows []CurrentWindow
}

// CurrentWindow is the newest stored state of one window.
type CurrentWindow struct {
	Window string
	Length time.Duration // zero = unknown, or not a time window
	// ResetsAt, Status, LimitReached, ReceivedAt and Source are the newest
	// report of the window's.
	ResetsAt     time.Time
	Status       string
	LimitReached bool
	ReceivedAt   time.Time
	Source       string
	// Percent (0-100) is the newest percent figure held for the window's
	// current period, with when it was received and its source. HasPercent
	// is false when none is held: no report carried one, or the newest one
	// was measured before the window reset.
	HasPercent        bool
	Percent           float64
	PercentReceivedAt time.Time
	PercentSource     string
}

// Current is the newest stored state of account at now. It reads this store
// only. A later report with no percent (a Claude Code "allowed" event) does
// not hide the newest percent figure; a percent figure measured before the
// window reset is not shown as the current period's use.
func (s *Store) Current(account string, now time.Time) (Current, error) {
	cur := Current{Account: account}
	if err := s.db.QueryRow(`SELECT backend FROM usage_reports WHERE account = ? ORDER BY received_at DESC, id DESC LIMIT 1`, account).Scan(&cur.Backend); err != nil && err != sql.ErrNoRows {
		return cur, fmt.Errorf("usagelog: newest report: %w", err)
	}
	rows, err := s.db.Query(`SELECT DISTINCT window FROM usage_windows WHERE account = ?`, account)
	if err != nil {
		return cur, fmt.Errorf("usagelog: windows: %w", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			_ = rows.Close()
			return cur, fmt.Errorf("usagelog: windows: %w", err)
		}
		names = append(names, n)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return cur, fmt.Errorf("usagelog: windows: %w", err)
	}
	for _, name := range names {
		newest, ok, err := s.newestWindow(account, name, false)
		if err != nil {
			return cur, err
		}
		if !ok {
			continue
		}
		cw := CurrentWindow{
			Window: name, Length: newest.length, ResetsAt: newest.resetsAt, Status: newest.status,
			LimitReached: newest.limitReached, ReceivedAt: newest.receivedAt, Source: newest.source,
		}
		if pct, ok, err := s.newestWindow(account, name, true); err != nil {
			return cur, err
		} else if ok && pct.samePeriod(newest, now) {
			cw.HasPercent, cw.Percent = true, pct.percent()
			cw.PercentReceivedAt, cw.PercentSource = pct.receivedAt, pct.source
		}
		if cw.Length == 0 {
			cw.Length = delegator.UsageWindowLength(name)
		}
		cur.Windows = append(cur.Windows, cw)
	}
	sort.SliceStable(cur.Windows, func(i, j int) bool {
		a, b := cur.Windows[i], cur.Windows[j]
		if (a.Length == 0) != (b.Length == 0) {
			return b.Length == 0 // windows with a length first
		}
		if a.Length != b.Length {
			return a.Length < b.Length
		}
		return a.Window < b.Window
	})
	return cur, nil
}

type windowRow struct {
	length       time.Duration
	used         sql.NullFloat64
	unit         string
	resetsAt     time.Time
	status       string
	limitReached bool
	receivedAt   time.Time
	source       string
}

func (w windowRow) percent() float64 {
	v := w.used.Float64
	if w.unit == delegator.UsageUnitFraction {
		v *= 100
	}
	return math.Round(v*100) / 100
}

// samePeriod reports whether w (a row with a percent) measures the period
// newest (the window's newest row) describes, at now: the window has not
// reset since w was measured.
func (w windowRow) samePeriod(newest windowRow, now time.Time) bool {
	if !w.resetsAt.IsZero() && !now.Before(w.resetsAt) {
		return false // the window reset after w was measured
	}
	if w.resetsAt.IsZero() || newest.resetsAt.IsZero() {
		return true
	}
	d := w.resetsAt.Sub(newest.resetsAt)
	return d <= sameWindowSlack && d >= -sameWindowSlack
}

// newestWindow is account's newest row of window (with a percent, when
// withUsed).
func (s *Store) newestWindow(account, window string, withUsed bool) (windowRow, bool, error) {
	q := `SELECT w.length_sec, w.used, w.used_unit, w.resets_at, w.status, w.limit_reached, w.received_at, r.source
		FROM usage_windows w JOIN usage_reports r ON r.id = w.report_id
		WHERE w.account = ? AND w.window = ?`
	if withUsed {
		q += ` AND w.used IS NOT NULL`
	}
	q += ` ORDER BY w.received_at DESC, w.id DESC LIMIT 1`
	var (
		row               windowRow
		lengthSec, resets int64
		reached           int
		received          int64
	)
	err := s.db.QueryRow(q, account, window).Scan(&lengthSec, &row.used, &row.unit, &resets, &row.status, &reached, &received, &row.source)
	if err == sql.ErrNoRows {
		return row, false, nil
	}
	if err != nil {
		return row, false, fmt.Errorf("usagelog: window %s: %w", window, err)
	}
	row.length = time.Duration(lengthSec) * time.Second
	if resets != 0 {
		row.resetsAt = time.Unix(resets, 0)
	}
	row.limitReached = reached != 0
	row.receivedAt = time.Unix(0, received)
	return row, true, nil
}

// ReportFromRead is the report of a usage read (a UsageQuery result) of
// source, or false when the read holds no figures received for it (a codex
// /usage with no fresh read repeats figures already stored). The payload is
// the read's Raw response when it has one, else nothing.
func ReportFromRead(info *delegator.UsageInfo, source string) (delegator.UsageReport, bool) {
	if !info.FreshRead() {
		return delegator.UsageReport{}, false
	}
	return delegator.UsageReport{Source: source, Windows: info.ReportWindows(), Payload: info.Raw}, true
}

// Binding is one agent's view of the store: the account its figures are
// stored under, its backend and the agent id. The gateway builds one per
// agent; a nil Store makes every method a no-op.
type Binding struct {
	Store   *Store
	Backend string // Spec.Name
	Account string
	AgentID string
	Now     func() time.Time // nil = time.Now
	// OnError is told of a failed write (nil = ignored).
	OnError func(error)
}

func (b *Binding) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Record stamps r with the binding's account, backend, agent and the time,
// and saves it.
func (b *Binding) Record(r delegator.UsageReport) {
	if b == nil || b.Store == nil {
		return
	}
	r.ReceivedAt = b.now()
	r.Backend, r.Account, r.AgentID = b.Backend, b.Account, b.AgentID
	if err := b.Store.Save(r); err != nil && b.OnError != nil {
		b.OnError(err)
	}
}

// RecordRead records a successful usage read of source (see ReportFromRead).
func (b *Binding) RecordRead(info *delegator.UsageInfo, source string) {
	if r, ok := ReportFromRead(info, source); ok {
		b.Record(r)
	}
}

// Current is the stored state of the binding's account now.
func (b *Binding) Current() (Current, time.Time, error) {
	if b == nil {
		return Current{}, time.Now(), nil
	}
	now := b.now()
	if b.Store == nil {
		return Current{Account: b.Account}, now, nil
	}
	c, err := b.Store.Current(b.Account, now)
	return c, now, err
}

// AccountKey is the account a usage report of agentID on backend is stored
// under: the backend's usage account (Spec.UsageAccount, #3104: a Claude
// Code login) when it has one, else the backend and agent (codex reads per
// agent). Two agents on one Claude Code login share a key; agents on
// different logins, and different codex agents, never do.
func AccountKey(backend, agentID, usageAccount string) string {
	if usageAccount != "" {
		return usageAccount
	}
	return backend + "/" + strings.TrimSpace(agentID)
}
