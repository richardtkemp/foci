package memory

import (
	"database/sql"
	"time"

	"foci/internal/sqlite"
)

// ScratchpadEntry is a key-value working state entry.
type ScratchpadEntry struct {
	Key     string
	Content string
	Updated time.Time
}

// ScratchpadListEntry is returned by List() with metadata about each key.
type ScratchpadListEntry struct {
	Key       string
	SizeBytes int
	Updated   time.Time
}

// Scratchpad stores working state that survives compaction.
// Entries are injected back into post-compaction context.
type Scratchpad struct {
	db *sql.DB
}

// NewScratchpad creates or opens the scratchpad store.
func NewScratchpad(dbPath string) (*Scratchpad, error) {
	db, err := sqlite.OpenInit(dbPath, `CREATE TABLE IF NOT EXISTS scratchpad (
		agent_id TEXT    NOT NULL DEFAULT '',
		key      TEXT    NOT NULL,
		content  TEXT    NOT NULL,
		updated  TEXT    NOT NULL,
		PRIMARY KEY (agent_id, key)
	)`)
	if err != nil {
		return nil, err
	}
	return &Scratchpad{db: db}, nil
}



// Write sets or overwrites a scratchpad entry for the given agent.
func (s *Scratchpad) Write(agentID, key, content string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO scratchpad (agent_id, key, content, updated) VALUES (?, ?, ?, ?)
		 ON CONFLICT(agent_id, key) DO UPDATE SET content = excluded.content, updated = excluded.updated`,
		agentID, key, content, now,
	)
	return err
}

// Read returns a scratchpad entry by key for the given agent. Returns empty string if not found.
func (s *Scratchpad) Read(agentID, key string) (string, error) {
	var content string
	err := s.db.QueryRow("SELECT content FROM scratchpad WHERE agent_id = ? AND key = ?", agentID, key).Scan(&content)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return content, err
}

// Clear removes a scratchpad entry by key for the given agent.
func (s *Scratchpad) Clear(agentID, key string) error {
	_, err := s.db.Exec("DELETE FROM scratchpad WHERE agent_id = ? AND key = ?", agentID, key)
	return err
}

// All returns all scratchpad entries for the given agent.
func (s *Scratchpad) All(agentID string) ([]ScratchpadEntry, error) {
	rows, err := s.db.Query("SELECT key, content, updated FROM scratchpad WHERE agent_id = ? ORDER BY key", agentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

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

// List returns all scratchpad keys with their sizes and last-modified times for the given agent.
func (s *Scratchpad) List(agentID string) ([]ScratchpadListEntry, error) {
	rows, err := s.db.Query("SELECT key, content, updated FROM scratchpad WHERE agent_id = ? ORDER BY key", agentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var entries []ScratchpadListEntry
	for rows.Next() {
		var e ScratchpadListEntry
		var content string
		var updated string
		if err := rows.Scan(&e.Key, &content, &updated); err != nil {
			return nil, err
		}
		e.SizeBytes = len(content)
		e.Updated, _ = time.Parse(time.RFC3339, updated)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// Close closes the underlying database.
func (s *Scratchpad) Close() error {
	return s.db.Close()
}
