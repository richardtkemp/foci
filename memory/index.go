package memory

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// Result is a single search result from the FTS5 index.
type Result struct {
	Path    string
	Snippet string
	Source  string  // "memory" or "conversation"
	Rank    float64
}

// Index manages an FTS5 full-text search index over memory files
// and conversation history. Memory files are weighted 2x higher
// than conversation entries.
type Index struct {
	db        *sql.DB
	memoryDir string
	mu        sync.Mutex
}

// NewIndex creates or opens an FTS5 index at dbPath, indexing .md files from memoryDir.
func NewIndex(dbPath, memoryDir string) (*Index, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open memory db: %w", err)
	}

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}

	_, err = db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
		content, path, source,
		tokenize='porter unicode61'
	)`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create FTS5 table: %w", err)
	}

	return &Index{db: db, memoryDir: memoryDir}, nil
}

// Reindex scans memory .md files and rebuilds the memory portion of the index.
// Conversation entries are preserved.
func (idx *Index) Reindex() error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Clear old memory entries, keep conversation
	if _, err := idx.db.Exec("DELETE FROM memory_fts WHERE source = 'memory'"); err != nil {
		return fmt.Errorf("clear memory entries: %w", err)
	}

	return filepath.Walk(idx.memoryDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil // skip unreadable files
		}
		if len(data) == 0 {
			return nil
		}

		relPath, _ := filepath.Rel(idx.memoryDir, path)
		_, err = idx.db.Exec(
			"INSERT INTO memory_fts (content, path, source) VALUES (?, ?, 'memory')",
			string(data), relPath,
		)
		return err
	})
}

// IndexConversation adds a conversation message to the FTS5 index.
func (idx *Index) IndexConversation(text, session string) {
	if text == "" {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()

	idx.db.Exec(
		"INSERT INTO memory_fts (content, path, source) VALUES (?, ?, 'conversation')",
		text, session,
	)
}

// Search queries the FTS5 index. Memory results are ranked 2x higher
// than conversation results.
func (idx *Index) Search(query string) ([]Result, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	rows, err := idx.db.Query(`
		SELECT path,
		       snippet(memory_fts, 0, '>', '<', '...', 40),
		       source,
		       CASE source WHEN 'memory' THEN rank * 2.0 ELSE rank END AS weighted_rank
		FROM memory_fts
		WHERE memory_fts MATCH ?
		ORDER BY weighted_rank
		LIMIT 20
	`, query)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var results []Result
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.Path, &r.Snippet, &r.Source, &r.Rank); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// Close closes the underlying database.
func (idx *Index) Close() error {
	return idx.db.Close()
}
