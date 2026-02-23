package memory

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Reminder is a deferred thought for later.
type Reminder struct {
	ID      int64
	Text    string
	DueAt   time.Time
	DueTag  string // original tag: "next_heartbeat", "tomorrow", etc.
	Created time.Time
}

// ReminderStore manages deferred thoughts in SQLite.
type ReminderStore struct {
	db *sql.DB
}

// NewReminderStore creates or opens the reminder store.
// Uses the same DB as the memory index if path matches.
func NewReminderStore(dbPath string) (*ReminderStore, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open reminder db: %w", err)
	}

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS reminders (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		text    TEXT    NOT NULL,
		due_at  TEXT    NOT NULL,
		due_tag TEXT    NOT NULL,
		created TEXT    NOT NULL
	)`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create reminders table: %w", err)
	}

	return &ReminderStore{db: db}, nil
}

// Add creates a new reminder. The when parameter is resolved to a concrete time:
//   - "next_heartbeat" → now (surfaced at next heartbeat)
//   - "tomorrow" → midnight tomorrow UTC
//   - "next_session" → now (surfaced at next message)
//   - YYYY-MM-DD → that date at midnight UTC
func (rs *ReminderStore) Add(text, when string) error {
	dueAt := resolveWhen(when)
	now := time.Now().UTC()

	_, err := rs.db.Exec(
		"INSERT INTO reminders (text, due_at, due_tag, created) VALUES (?, ?, ?, ?)",
		text, dueAt.Format(time.RFC3339), when, now.Format(time.RFC3339),
	)
	return err
}

// Due returns all reminders that are due (due_at <= now).
func (rs *ReminderStore) Due() ([]Reminder, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	rows, err := rs.db.Query(
		"SELECT id, text, due_at, due_tag, created FROM reminders WHERE due_at <= ? ORDER BY due_at",
		now,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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

// DismissAll removes all due reminders.
func (rs *ReminderStore) DismissAll() error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := rs.db.Exec("DELETE FROM reminders WHERE due_at <= ?", now)
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
	case "next_heartbeat", "next_session", "now":
		return now
	case "tomorrow":
		tomorrow := now.Add(24 * time.Hour)
		return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 0, 0, 0, 0, time.UTC)
	default:
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
