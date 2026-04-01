package tooldetail

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/sqlite"
	"foci/internal/timeutil"
)

// Compile-time check: Store satisfies platform.ToolDetailStore.
var _ platform.ToolDetailStore = (*Store)(nil)

const (
	toolDetailTTL     = 48 * time.Hour
	vacuumIdleMinutes = 10 // run cleanup when user idle > this many minutes
)

// Entry holds the fields returned by LoadAll for each persisted tool detail.
type Entry struct {
	CompactText string
	FullInput   string
	Result      string
}

// Store persists tool call detail text to SQLite so inline keyboard
// expansions survive restarts. Entries older than 48h are expired on cleanup.
type Store struct {
	db     *sql.DB
	mu     sync.Mutex // serialise writes (reads use db concurrency)
	closed bool
}

// NewStore opens (or creates) the SQLite database for tool call details.
// Sets PRAGMA auto_vacuum=INCREMENTAL so incremental_vacuum reclaims space.
func NewStore(dbPath string) (*Store, error) {
	db, err := sqlite.OpenInit(dbPath,
		"PRAGMA auto_vacuum=INCREMENTAL",
		`CREATE TABLE IF NOT EXISTS tool_call_details (
			message_id  INTEGER PRIMARY KEY,
			compact_text TEXT NOT NULL,
			full_input   TEXT NOT NULL,
			result       TEXT NOT NULL,
			created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tool_details_created_unix ON tool_call_details(unixepoch(created_at))`,
	)
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

// Store inserts or replaces a tool call detail entry.
func (s *Store) Store(messageID int64, compact, fullInput, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}

	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO tool_call_details (message_id, compact_text, full_input, result, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		messageID, compact, fullInput, result, timeutil.FormatNano(timeutil.Now()))
	if err != nil {
		log.Warnf("tooldetail", "store message_id=%d: %v", messageID, err)
	}
}

// LoadAll returns all entries newer than 48h. Used on startup to populate the in-memory map.
func (s *Store) LoadAll() (map[int64]Entry, error) {
	cutoff := timeutil.FormatNano(time.Now().Add(-toolDetailTTL))

	rows, err := s.db.Query(
		`SELECT message_id, compact_text, full_input, result FROM tool_call_details WHERE unixepoch(created_at) > unixepoch(?)`,
		cutoff)
	if err != nil {
		return nil, fmt.Errorf("query tool details: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[int64]Entry)
	for rows.Next() {
		var id int64
		var entry Entry
		if err := rows.Scan(&id, &entry.CompactText, &entry.FullInput, &entry.Result); err != nil {
			log.Warnf("tooldetail", "scan row: %v", err)
			continue
		}
		result[id] = entry
	}
	return result, rows.Err()
}

// ExpireAndVacuum deletes entries older than 48h and runs incremental vacuum.
func (s *Store) ExpireAndVacuum() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}

	cutoff := timeutil.FormatNano(time.Now().Add(-toolDetailTTL))
	res, err := s.db.Exec(`DELETE FROM tool_call_details WHERE unixepoch(created_at) <= unixepoch(?)`, cutoff)
	if err != nil {
		log.Warnf("tooldetail", "expire: %v", err)
		return
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		log.Infof("tooldetail", "expired %d old tool detail entries", n)
	}

	if _, err := s.db.Exec("PRAGMA incremental_vacuum"); err != nil {
		log.Warnf("tooldetail", "incremental_vacuum: %v", err)
	}
}

// Count returns the number of entries in the store. Test helper.
func (s *Store) Count() int {
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM tool_call_details").Scan(&n); err != nil {
		log.Warnf("tooldetail", "count: %v", err)
	}
	return n
}
