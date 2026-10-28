package memory

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"foci/internal/session"
	"foci/internal/sqlite"
	"foci/internal/timeutil"
)

// migrateLegacyReminderKeys rewrites reminders.session_key values from the
// legacy versioned form to the stable identity form. No-op once migrated.
func migrateLegacyReminderKeys(db *sql.DB) {
	rows, err := db.Query(`SELECT id, session_key FROM reminders WHERE session_key != ''`)
	if err != nil {
		return
	}
	type rekey struct {
		id     int64
		stable string
	}
	var rekeys []rekey
	for rows.Next() {
		var id int64
		var sk string
		if rows.Scan(&id, &sk) != nil {
			continue
		}
		if stable, ok := session.LegacyKeyToStable(sk); ok {
			rekeys = append(rekeys, rekey{id: id, stable: stable})
		}
	}
	_ = rows.Close()
	for _, rk := range rekeys {
		_, _ = db.Exec(`UPDATE reminders SET session_key = ? WHERE id = ?`, rk.stable, rk.id)
	}
}

// Reminder is a deferred thought for later.
type Reminder struct {
	ID         int64
	Text       string
	DueAt      time.Time
	DueTag     string // original tag: "tomorrow", "2h", etc.
	Created    time.Time
	SessionKey string // originating session (wake reminders only; empty for legacy rows)
}

// ReminderStore manages deferred thoughts in SQLite.
type ReminderStore struct {
	db *sql.DB

	// firedMu guards fired: the in-process overlay of wakes whose timer has
	// fired but whose turn has not run yet (#2233). It is runtime state on top
	// of the persisted rows — deliberately not persisted, because a restart's
	// restore re-fires every pending row, so "fired and waiting" is by
	// definition empty at startup.
	firedMu sync.Mutex
	fired   map[int64]string // wake id → resolved session key

	// onChange, when set, runs after any change to the set or state of
	// pending wakes: one added, fired, un-fired or removed. Wired at gateway
	// setup so the app's per-chat reminder indicator tracks the store without
	// polling (#2600). Passive reminders do not fire it: the app never shows
	// them. Guarded by firedMu.
	onChange func()
}

// Wake states (#2233): a wake is pending until its timer fires; from firing
// until its turn runs it is held (behind background work) or fired (waiting
// to run); once the turn runs the row is dismissed. Shared by the remind
// tool's list and the app's reminder indicator (#2600).
const (
	WakeStatePending = "pending"
	WakeStateHeld    = "held"
	WakeStateFired   = "fired"
)

// SetOnChange wires the callback run after every pending-wake change (see
// the onChange field). nil unwires it.
func (rs *ReminderStore) SetOnChange(fn func()) {
	rs.firedMu.Lock()
	rs.onChange = fn
	rs.firedMu.Unlock()
}

// notifyChange runs the onChange callback, if wired, outside any lock.
func (rs *ReminderStore) notifyChange() {
	rs.firedMu.Lock()
	fn := rs.onChange
	rs.firedMu.Unlock()
	if fn != nil {
		fn()
	}
}

// WakeState classifies wake id against the fired overlay: WakeStatePending
// until its timer fires, then WakeStateHeld when held reports its session
// held behind background work, else WakeStateFired. A nil held reads every
// fired wake as WakeStateFired.
func (rs *ReminderStore) WakeState(id int64, held func(sessionKey string) bool) string {
	sk, fired := rs.FiredWake(id)
	if !fired {
		return WakeStatePending
	}
	if held != nil && held(sk) {
		return WakeStateHeld
	}
	return WakeStateFired
}

// MarkWakeFired records that wake id's timer has fired on sessionKey and its
// turn is waiting to run — the runtime overlay that lets the remind tool
// report "fired, held behind background work" / "fired, waiting to run"
// instead of a misleading "overdue" while the wake sits behind background
// work (#2233). Cleared by ClearWakeFired when the turn runs or the injection
// is refused; Dismiss clears it with the row.
func (rs *ReminderStore) MarkWakeFired(id int64, sessionKey string) {
	rs.firedMu.Lock()
	if rs.fired == nil {
		rs.fired = make(map[int64]string)
	}
	rs.fired[id] = sessionKey
	rs.firedMu.Unlock()
	rs.notifyChange()
}

// ClearWakeFired drops the fired marker for id — its turn has run, its
// injection was refused, or the wake is otherwise no longer waiting.
func (rs *ReminderStore) ClearWakeFired(id int64) {
	rs.firedMu.Lock()
	_, was := rs.fired[id]
	delete(rs.fired, id)
	rs.firedMu.Unlock()
	if was {
		rs.notifyChange()
	}
}

// FiredWake reports the session key wake id fired on and whether it is still
// waiting for its turn.
func (rs *ReminderStore) FiredWake(id int64) (sessionKey string, ok bool) {
	rs.firedMu.Lock()
	defer rs.firedMu.Unlock()
	sk, ok := rs.fired[id]
	return sk, ok
}

// NewReminderStore creates or opens the reminder store.
// Uses the same DB as the memory index if path matches.
func NewReminderStore(dbPath string) (*ReminderStore, error) {
	db, err := sqlite.OpenInit(dbPath, `CREATE TABLE IF NOT EXISTS reminders (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		agent_id TEXT    NOT NULL DEFAULT '',
		text     TEXT    NOT NULL,
		due_at   TEXT    NOT NULL,
		due_tag  TEXT    NOT NULL,
		created  TEXT    NOT NULL
	)`)
	if err != nil {
		return nil, err
	}

	// Idempotent migration: add wake column for active wake reminders.
	_, err = db.Exec(`ALTER TABLE reminders ADD COLUMN wake INTEGER NOT NULL DEFAULT 0`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column") {
		_ = db.Close()
		return nil, fmt.Errorf("add wake column: %w", err)
	}

	// Idempotent migration: add session_key for platform-aware wake routing.
	_, err = db.Exec(`ALTER TABLE reminders ADD COLUMN session_key TEXT NOT NULL DEFAULT ''`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column") {
		_ = db.Close()
		return nil, fmt.Errorf("add session_key column: %w", err)
	}

	// Expression index for correct cross-timezone due_at ordering.
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_reminders_due_unix ON reminders(unixepoch(due_at))`)

	// Idempotent legacy migration: re-key wake reminders that captured a
	// pre-stable-identity session key (with a version segment), so pending
	// wakes still target the right session after the key format change.
	migrateLegacyReminderKeys(db)

	return &ReminderStore{db: db}, nil
}

// Add creates a new reminder. The when parameter is resolved to a concrete time:
//   - "now" → now (surfaced at the next message to a root chat)
//   - "tomorrow" → midnight tomorrow (local time)
//   - YYYY-MM-DD → that date at midnight (local time)
func (rs *ReminderStore) Add(agentID, text, when string) error {
	dueAt := resolveWhen(when)
	now := timeutil.Now()

	_, err := rs.db.Exec(
		"INSERT INTO reminders (agent_id, text, due_at, due_tag, created) VALUES (?, ?, ?, ?, ?)",
		agentID, text, timeutil.Format(dueAt), when, timeutil.Format(now),
	)
	return err
}

// AddWake creates a wake reminder (wake=1) and returns its row ID.
// sessionKey records which session the reminder was created from so
// the wake fires on the correct platform.
func (rs *ReminderStore) AddWake(agentID, sessionKey, text, when string) (int64, error) {
	dueAt := resolveWhen(when)
	now := timeutil.Now()

	result, err := rs.db.Exec(
		"INSERT INTO reminders (agent_id, text, due_at, due_tag, created, wake, session_key) VALUES (?, ?, ?, ?, ?, 1, ?)",
		agentID, text, timeutil.Format(dueAt), when, timeutil.Format(now), sessionKey,
	)
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err == nil {
		rs.notifyChange()
	}
	return id, err
}

// PendingWakes returns all wake reminders for the given agent, ordered by due time.
func (rs *ReminderStore) PendingWakes(agentID string) ([]Reminder, error) {
	rows, err := rs.db.Query(
		"SELECT id, text, due_at, due_tag, created, session_key FROM reminders WHERE agent_id = ? AND wake = 1 ORDER BY unixepoch(due_at)",
		agentID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var reminders []Reminder
	for rows.Next() {
		var r Reminder
		var dueAt, created string
		if err := rows.Scan(&r.ID, &r.Text, &dueAt, &r.DueTag, &created, &r.SessionKey); err != nil {
			return nil, err
		}
		r.DueAt, _ = time.Parse(time.RFC3339, dueAt)
		r.Created, _ = time.Parse(time.RFC3339, created)
		reminders = append(reminders, r)
	}
	return reminders, rows.Err()
}

// Due returns all passive reminders for the given agent that are due (due_at <= now).
// Wake reminders are excluded — they fire via their own timer.
func (rs *ReminderStore) Due(agentID string) ([]Reminder, error) {
	now := timeutil.Format(timeutil.Now())

	rows, err := rs.db.Query(
		"SELECT id, text, due_at, due_tag, created FROM reminders WHERE agent_id = ? AND unixepoch(due_at) <= unixepoch(?) AND wake = 0 ORDER BY unixepoch(due_at)",
		agentID, now,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var reminders []Reminder
	for rows.Next() {
		var r Reminder
		var dueAt, created string
		if err := rows.Scan(&r.ID, &r.Text, &dueAt, &r.DueTag, &created); err != nil {
			return nil, err
		}
		r.DueAt, _ = time.Parse(time.RFC3339, dueAt)
		r.Created, _ = time.Parse(time.RFC3339, created)
		reminders = append(reminders, r)
	}
	return reminders, rows.Err()
}

// Dismiss removes a reminder by ID. A wake's fired marker goes with the row —
// its turn ran (the wake path dismisses on completion) or it was cancelled.
func (rs *ReminderStore) Dismiss(id int64) error {
	rs.firedMu.Lock()
	delete(rs.fired, id)
	rs.firedMu.Unlock()
	res, err := rs.db.Exec("DELETE FROM reminders WHERE id = ?", id)
	if err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			rs.notifyChange()
		}
	}
	return err
}

// DismissAll removes all due passive reminders for the given agent.
// Wake reminders are excluded — they are dismissed explicitly by ID when they fire.
// The fired overlay is left untouched: it only ever holds wake ids (#2233), and
// those rows survive this call, so wiping the overlay would make a fired wake
// held behind background work read as "overdue" again the moment any passive
// reminder surfaces (a turn calling DismissAll).
func (rs *ReminderStore) DismissAll(agentID string) error {
	now := timeutil.Format(timeutil.Now())
	_, err := rs.db.Exec("DELETE FROM reminders WHERE agent_id = ? AND unixepoch(due_at) <= unixepoch(?) AND wake = 0", agentID, now)
	return err
}

// Close closes the underlying database.
func (rs *ReminderStore) Close() error {
	return rs.db.Close()
}

// resolveWhen converts a human tag to a concrete time.
func resolveWhen(when string) time.Time {
	now := timeutil.Now()

	switch when {
	case "now":
		return now
	case "tomorrow":
		tomorrow := now.Add(24 * time.Hour)
		return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 0, 0, 0, 0, now.Location())
	default:
		// Try parsing as an ISO 8601 / RFC3339 timestamp
		if t, err := time.Parse(time.RFC3339, when); err == nil {
			return t
		}
		// Try parsing as a date (midnight local time)
		if t, err := time.ParseInLocation("2006-01-02", when, now.Location()); err == nil {
			return t
		}
		// Try parsing as a duration
		if d, err := time.ParseDuration(when); err == nil {
			return now.Add(d)
		}
		// Default: immediate
		return now
	}
}
