package memory

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"foci/internal/sqlite"
)

// Reminder is a deferred thought for later.
type Reminder struct {
	ID      int64
	Text    string
	DueAt   time.Time
	DueTag  string // original tag: "next_keepalive", "tomorrow", etc.
	Created time.Time
}

// ReminderStore manages deferred thoughts in SQLite.
type ReminderStore struct {
	db *sql.DB
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

	return &ReminderStore{db: db}, nil
}

// Add creates a new reminder. The when parameter is resolved to a concrete time:
//   - "next_keepalive" → now (surfaced at next keepalive)
//   - "tomorrow" → midnight tomorrow UTC
//   - "next_session" → now (surfaced at next message)
//   - YYYY-MM-DD → that date at midnight UTC
func (rs *ReminderStore) Add(agentID, text, when string) error {
	dueAt := resolveWhen(when)
	now := time.Now().UTC()

	_, err := rs.db.Exec(
		"INSERT INTO reminders (agent_id, text, due_at, due_tag, created) VALUES (?, ?, ?, ?, ?)",
		agentID, text, dueAt.Format(time.RFC3339), when, now.Format(time.RFC3339),
	)
	return err
}

// AddWake creates a wake reminder (wake=1) and returns its row ID.
func (rs *ReminderStore) AddWake(agentID, text, when string) (int64, error) {
	dueAt := resolveWhen(when)
	now := time.Now().UTC()

	result, err := rs.db.Exec(
		"INSERT INTO reminders (agent_id, text, due_at, due_tag, created, wake) VALUES (?, ?, ?, ?, ?, 1)",
		agentID, text, dueAt.Format(time.RFC3339), when, now.Format(time.RFC3339),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// PendingWakes returns all wake reminders for the given agent, ordered by due time.
func (rs *ReminderStore) PendingWakes(agentID string) ([]Reminder, error) {
	rows, err := rs.db.Query(
		"SELECT id, text, due_at, due_tag, created FROM reminders WHERE agent_id = ? AND wake = 1 ORDER BY due_at",
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
		if err := rows.Scan(&r.ID, &r.Text, &dueAt, &r.DueTag, &created); err != nil {
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
	now := time.Now().UTC().Format(time.RFC3339)

	rows, err := rs.db.Query(
		"SELECT id, text, due_at, due_tag, created FROM reminders WHERE agent_id = ? AND due_at <= ? AND wake = 0 ORDER BY due_at",
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

// Dismiss removes a reminder by ID.
func (rs *ReminderStore) Dismiss(id int64) error {
	_, err := rs.db.Exec("DELETE FROM reminders WHERE id = ?", id)
	return err
}

// DismissAll removes all due passive reminders for the given agent.
// Wake reminders are excluded — they are dismissed explicitly by ID when they fire.
func (rs *ReminderStore) DismissAll(agentID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := rs.db.Exec("DELETE FROM reminders WHERE agent_id = ? AND due_at <= ? AND wake = 0", agentID, now)
	return err
}

// Close closes the underlying database.
func (rs *ReminderStore) Close() error {
	return rs.db.Close()
}

// resolveWhen converts a human tag to a concrete time.
func resolveWhen(when string) time.Time {
	now := time.Now().UTC()

	switch when {
	case "next_keepalive", "next_heartbeat", "next_session", "now":
		return now
	case "tomorrow":
		tomorrow := now.Add(24 * time.Hour)
		return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 0, 0, 0, 0, time.UTC)
	default:
		// Try parsing as an ISO 8601 / RFC3339 timestamp
		if t, err := time.Parse(time.RFC3339, when); err == nil {
			return t
		}
		// Try parsing as a date
		if t, err := time.Parse("2006-01-02", when); err == nil {
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
