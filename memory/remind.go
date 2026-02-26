package memory

import (
	"database/sql"
	"fmt"
	"time"

	"clod/log"

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

	if err := migrateReminders(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate reminders: %w", err)
	}

	return &ReminderStore{db: db}, nil
}

// migrateReminders handles schema evolution for the reminders table.
func migrateReminders(db *sql.DB) error {
	// Check if table exists
	var name string
	err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='reminders'").Scan(&name)
	if err == sql.ErrNoRows {
		// Fresh install
		_, err := db.Exec(`CREATE TABLE reminders (
			id       INTEGER PRIMARY KEY AUTOINCREMENT,
			agent_id TEXT    NOT NULL DEFAULT '',
			text     TEXT    NOT NULL,
			due_at   TEXT    NOT NULL,
			due_tag  TEXT    NOT NULL,
			created  TEXT    NOT NULL
		)`)
		return err
	}

	// Table exists — check if agent_id column is present
	var hasAgentID bool
	rows, err := db.Query("PRAGMA table_info(reminders)")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var cname, ctype string
		var notnull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &cname, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return err
		}
		if cname == "agent_id" {
			hasAgentID = true
		}
	}

	if hasAgentID {
		return nil // already migrated
	}

	// Add agent_id column with default empty string (migrates existing rows)
	log.Infof("reminders", "migrating reminders table to add agent_id column")
	_, err = db.Exec(`ALTER TABLE reminders ADD COLUMN agent_id TEXT NOT NULL DEFAULT ''`)
	return err
}

// Add creates a new reminder. The when parameter is resolved to a concrete time:
//   - "next_heartbeat" → now (surfaced at next heartbeat)
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

// Due returns all reminders for the given agent that are due (due_at <= now).
func (rs *ReminderStore) Due(agentID string) ([]Reminder, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	rows, err := rs.db.Query(
		"SELECT id, text, due_at, due_tag, created FROM reminders WHERE agent_id = ? AND due_at <= ? ORDER BY due_at",
		agentID, now,
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

// DismissAll removes all due reminders for the given agent.
func (rs *ReminderStore) DismissAll(agentID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := rs.db.Exec("DELETE FROM reminders WHERE agent_id = ? AND due_at <= ?", agentID, now)
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
