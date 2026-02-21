package memory

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// ScratchpadEntry is a key-value working state entry.
type ScratchpadEntry struct {
	Key     string
	Content string
	Updated time.Time
}

// Scratchpad stores working state that survives compaction.
// Entries are injected back into post-compaction context.
type Scratchpad struct {
	db *sql.DB
}

// NewScratchpad creates or opens the scratchpad store.
func NewScratchpad(dbPath string) (*Scratchpad, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open scratchpad db: %w", err)
	}

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS scratchpad (
		key     TEXT PRIMARY KEY,
		content TEXT    NOT NULL,
		updated TEXT    NOT NULL
	)`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create scratchpad table: %w", err)
	}

	return &Scratchpad{db: db}, nil
}

// Write sets or overwrites a scratchpad entry.
func (s *Scratchpad) Write(key, content string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO scratchpad (key, content, updated) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET content = excluded.content, updated = excluded.updated`,
		key, content, now,
	)
	return err
}

// Read returns a scratchpad entry by key. Returns empty string if not found.
func (s *Scratchpad) Read(key string) (string, error) {
	var content string
	err := s.db.QueryRow("SELECT content FROM scratchpad WHERE key = ?", key).Scan(&content)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return content, err
}

// Clear removes a scratchpad entry by key.
func (s *Scratchpad) Clear(key string) error {
	_, err := s.db.Exec("DELETE FROM scratchpad WHERE key = ?", key)
	return err
}

// All returns all scratchpad entries.
func (s *Scratchpad) All() ([]ScratchpadEntry, error) {
	rows, err := s.db.Query("SELECT key, content, updated FROM scratchpad ORDER BY key")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []ScratchpadEntry
	for rows.Next() {
		var e ScratchpadEntry
		var updated string
		if err := rows.Scan(&e.Key, &e.Content, &updated); err != nil {
			return nil, err
		}
		e.Updated, _ = time.Parse(time.RFC3339, updated)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// Close closes the underlying database.
func (s *Scratchpad) Close() error {
	return s.db.Close()
}
