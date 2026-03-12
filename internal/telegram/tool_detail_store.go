package telegram

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/sqlite"
)

// Compile-time check: ToolDetailStore satisfies platform.ToolDetailStore.
var _ platform.ToolDetailStore = (*ToolDetailStore)(nil)

const (
	toolDetailTTL     = 48 * time.Hour
	vacuumIdleMinutes = 10 // run cleanup when user idle > this many minutes
)

// ToolDetailStore persists tool call detail text to SQLite so inline keyboard
// expansions survive restarts. Entries older than 48h are expired on cleanup.
type ToolDetailStore struct {
	db     *sql.DB
	mu     sync.Mutex // serialise writes (reads use db concurrency)
	closed bool
}

// NewToolDetailStore opens (or creates) the SQLite database for tool call details.
// Sets PRAGMA auto_vacuum=INCREMENTAL so incremental_vacuum reclaims space.
func NewToolDetailStore(dbPath string) (*ToolDetailStore, error) {
	db, err := sqlite.OpenInit(dbPath,
		"PRAGMA auto_vacuum=INCREMENTAL",
		`CREATE TABLE IF NOT EXISTS tool_call_details (
			message_id  INTEGER PRIMARY KEY,
			compact_text TEXT NOT NULL,
			full_input   TEXT NOT NULL,
			result       TEXT NOT NULL,
			created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		)`,
	)
	if err != nil {
		return nil, err
	}
	return &ToolDetailStore{db: db}, nil
}

// Close closes the underlying database.
func (s *ToolDetailStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

// Store inserts or replaces a tool call detail entry.
func (s *ToolDetailStore) Store(messageID int64, compact, fullInput, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}

	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO tool_call_details (message_id, compact_text, full_input, result, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		messageID, compact, fullInput, result, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		log.Warnf("tool_detail_store", "store message_id=%d: %v", messageID, err)
	}
}

// LoadAll returns all entries newer than 48h. Used on startup to populate the in-memory map.
func (s *ToolDetailStore) LoadAll() (map[int64]toolResultEntry, error) {
	cutoff := time.Now().Add(-toolDetailTTL).UTC().Format(time.RFC3339Nano)

	rows, err := s.db.Query(
		`SELECT message_id, compact_text, full_input, result FROM tool_call_details WHERE created_at > ?`,
		cutoff)
	if err != nil {
		return nil, fmt.Errorf("query tool details: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[int64]toolResultEntry)
	for rows.Next() {
		var id int64
		var entry toolResultEntry
		if err := rows.Scan(&id, &entry.compactText, &entry.fullInput, &entry.result); err != nil {
			log.Warnf("tool_detail_store", "scan row: %v", err)
			continue
		}
		result[id] = entry
	}
	return result, rows.Err()
}

// ExpireAndVacuum deletes entries older than 48h and runs incremental vacuum.
func (s *ToolDetailStore) ExpireAndVacuum() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}

	cutoff := time.Now().Add(-toolDetailTTL).UTC().Format(time.RFC3339Nano)
	res, err := s.db.Exec(`DELETE FROM tool_call_details WHERE created_at <= ?`, cutoff)
	if err != nil {
		log.Warnf("tool_detail_store", "expire: %v", err)
		return
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		log.Infof("tool_detail_store", "expired %d old tool detail entries", n)
	}

	if _, err := s.db.Exec("PRAGMA incremental_vacuum"); err != nil {
		log.Warnf("tool_detail_store", "incremental_vacuum: %v", err)
	}
}

// Count returns the number of entries in the store. Test helper.
func (s *ToolDetailStore) Count() int {
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM tool_call_details").Scan(&n); err != nil {
		log.Warnf("tool_detail_store", "count: %v", err)
	}
	return n
}
