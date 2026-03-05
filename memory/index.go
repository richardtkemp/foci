package memory

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"foci/log"

	"github.com/fsnotify/fsnotify"
	_ "modernc.org/sqlite"
)

// Result is a single search result from the FTS5 index.
type Result struct {
	Path    string
	Snippet string
	Source  string // source name (e.g., "memory", "code", "docs") or "conversation"
	Rank    float64
}

// SourceConfig describes a memory source directory with weight.
type SourceConfig struct {
	Dir    string
	Weight float64
}

// Index manages an FTS5 full-text search index over memory files
// and conversation history. Multiple sources can be indexed, each
// with a configurable weight multiplier.
type Index struct {
	db                 *sql.DB
	sources            map[string]SourceConfig // name -> {dir, weight}
	conversationWeight float64                 // weight multiplier for conversation results
	watcher            *fsnotify.Watcher
	debounce           time.Duration
	reindexTimer       *time.Timer
	sweepStop          chan struct{} // closed to stop sweep goroutine
	mu                 sync.Mutex
}

// NewIndex creates or opens an FTS5 index at dbPath, indexing .md files from the given sources.
// sources maps source names to {dir, weight}. debounce is the delay before auto-reindexing
// on file change (0s = immediate). conversationWeight is the multiplier for conversation search results.
func NewIndex(dbPath string, sources map[string]SourceConfig, debounce time.Duration, conversationWeight float64) (*Index, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open memory db: %w", err)
	}

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}

	_, err = db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
		content, path, source,
		tokenize='porter unicode61'
	)`)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create FTS5 table: %w", err)
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS memory_meta (
		source TEXT NOT NULL,
		path TEXT NOT NULL,
		mtime REAL NOT NULL,
		PRIMARY KEY (source, path)
	)`)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create memory_meta table: %w", err)
	}

	return &Index{
		db:                 db,
		sources:            sources,
		conversationWeight: conversationWeight,
		debounce:           debounce,
	}, nil
}

// Reindex scans all configured source directories and rebuilds the memory portion of the index.
// Conversation entries are preserved.
func (idx *Index) Reindex() error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Clear old entries for all configured sources, keep conversation
	var sourceNames []string
	for name := range idx.sources {
		sourceNames = append(sourceNames, name)
	}

	for _, name := range sourceNames {
		if _, err := idx.db.Exec("DELETE FROM memory_fts WHERE source = ?", name); err != nil {
			return fmt.Errorf("clear entries for source %q: %w", name, err)
		}
		if _, err := idx.db.Exec("DELETE FROM memory_meta WHERE source = ?", name); err != nil {
			return fmt.Errorf("clear meta for source %q: %w", name, err)
		}
	}

	// Index each source directory
	for sourceName, sourceCfg := range idx.sources {
		if err := filepath.Walk(sourceCfg.Dir, func(path string, info os.FileInfo, err error) error {
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

			relPath, _ := filepath.Rel(sourceCfg.Dir, path)
			_, err = idx.db.Exec(
				"INSERT INTO memory_fts (content, path, source) VALUES (?, ?, ?)",
				string(data), relPath, sourceName,
			)
			if err != nil {
				return err
			}
			_, err = idx.db.Exec(
				"INSERT OR REPLACE INTO memory_meta (source, path, mtime) VALUES (?, ?, ?)",
				sourceName, relPath, float64(info.ModTime().Unix()),
			)
			return err
		}); err != nil {
			return fmt.Errorf("index source %q: %w", sourceName, err)
		}
	}

	return nil
}

// IndexConversation adds a conversation message to the FTS5 index.
func (idx *Index) IndexConversation(text, session string) {
	if text == "" {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()

	if _, err := idx.db.Exec(
		"INSERT INTO memory_fts (content, path, source) VALUES (?, ?, 'conversation')",
		text, session,
	); err != nil {
		log.Errorf("memory", "index conversation: %v", err)
		return
	}
	if _, err := idx.db.Exec(
		"INSERT OR REPLACE INTO memory_meta (source, path, mtime) VALUES ('conversation', ?, ?)",
		session, float64(time.Now().Unix()),
	); err != nil {
		log.Errorf("memory", "index conversation meta: %v", err)
	}
}

// buildWeightedRankCase constructs a CASE statement for per-source weight multipliers.
// Each source gets: rank * (1.0 + weight). Conversation gets 1.0x (no multiplier).
func (idx *Index) buildWeightedRankCase() string {
	var cases []string
	for name, cfg := range idx.sources {
		multiplier := 1.0 + cfg.Weight
		cases = append(cases, fmt.Sprintf("WHEN '%s' THEN rank * %.2f", name, multiplier))
	}
	// Conversation is fallback — only surfaces when memory files don't match
	cases = append(cases, fmt.Sprintf("WHEN 'conversation' THEN rank * %.2f", idx.conversationWeight))
	cases = append(cases, "ELSE rank")

	return "CASE source " + strings.Join(cases, " ") + " END"
}

// Search queries the FTS5 index. sort controls result ordering:
// "relevance" (default/empty) orders by weighted rank,
// "newest" orders by file mtime descending, "oldest" orders by mtime ascending.
func (idx *Index) Search(query string, sort string) ([]Result, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var sqlStr string
	switch sort {
	case "newest", "oldest":
		order := "DESC"
		if sort == "oldest" {
			order = "ASC"
		}
		sqlStr = fmt.Sprintf(`
			SELECT f.path,
			       snippet(memory_fts, 0, '>', '<', '...', 40),
			       f.source,
			       COALESCE(m.mtime, 0) AS mtime
			FROM memory_fts f
			LEFT JOIN memory_meta m ON f.source = m.source AND f.path = m.path
			WHERE memory_fts MATCH ?
			ORDER BY mtime %s
			LIMIT 20
		`, order)
	default:
		weightedRankCase := idx.buildWeightedRankCase()
		sqlStr = fmt.Sprintf(`
			SELECT path,
			       snippet(memory_fts, 0, '>', '<', '...', 40),
			       source,
			       %s AS weighted_rank
			FROM memory_fts
			WHERE memory_fts MATCH ?
			ORDER BY weighted_rank
			LIMIT 20
		`, weightedRankCase)
	}

	rows, err := idx.db.Query(sqlStr, query)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer func() { _ = rows.Close() }()

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

// Watch starts file system watching on all source directories.
// When .md files change, triggers a reindex after the debounce delay.
func (idx *Index) Watch() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}

	idx.mu.Lock()
	idx.watcher = watcher
	idx.mu.Unlock()

	// Add all source directories to watcher
	for _, cfg := range idx.sources {
		if err := watcher.Add(cfg.Dir); err != nil {
			_ = watcher.Close()
			return fmt.Errorf("watch %s: %w", cfg.Dir, err)
		}
	}

	// Start event handler goroutine
	go idx.handleFileEvents()
	return nil
}

// handleFileEvents processes file system events from the watcher.
func (idx *Index) handleFileEvents() {
	for {
		select {
		case event, ok := <-idx.watcher.Events:
			if !ok {
				return
			}
			// Only reindex on Write/Create/Remove for .md files
			if filepath.Ext(event.Name) == ".md" {
				idx.scheduleReindex()
			}
		case err, ok := <-idx.watcher.Errors:
			if !ok {
				return
			}
			log.Warnf("memory", "file watcher error: %v", err)
		}
	}
}

// scheduleReindex debounces reindex requests. Multiple file changes
// within the debounce window will trigger only one reindex.
func (idx *Index) scheduleReindex() {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Cancel existing timer if any
	if idx.reindexTimer != nil {
		idx.reindexTimer.Stop()
	}

	// Schedule reindex after debounce delay
	idx.reindexTimer = time.AfterFunc(idx.debounce, func() {
		if err := idx.Reindex(); err != nil {
			log.Errorf("memory", "auto-reindex failed: %v", err)
		}
	})
}

// StartSweep launches a background goroutine that calls Reindex periodically.
// The first sweep fires after initial, then repeats every interval.
// Call Close to stop the goroutine.
func (idx *Index) StartSweep(initial, interval time.Duration) {
	idx.mu.Lock()
	idx.sweepStop = make(chan struct{})
	stop := idx.sweepStop
	idx.mu.Unlock()

	go runSweepLoop(stop, initial, interval, "sweep", idx.Reindex)
}

// runSweepLoop executes periodic reindexing after an initial delay.
// Used by both Index and BleveIndex.
func runSweepLoop(stop <-chan struct{}, initial, interval time.Duration, logPrefix string, reindexFn func() error) {
	select {
	case <-time.After(initial):
	case <-stop:
		return
	}
	log.Infof("memory", "%s: initial reindex", logPrefix)
	if err := reindexFn(); err != nil {
		log.Errorf("memory", "%s reindex: %v", logPrefix, err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			log.Infof("memory", "%s: periodic reindex", logPrefix)
			if err := reindexFn(); err != nil {
				log.Errorf("memory", "%s reindex: %v", logPrefix, err)
			}
		case <-stop:
			return
		}
	}
}

// Close closes the watcher, stops the sweep goroutine, and closes the database.
func (idx *Index) Close() error {
	idx.mu.Lock()
	if idx.sweepStop != nil {
		close(idx.sweepStop)
		idx.sweepStop = nil
	}
	if idx.watcher != nil {
		_ = idx.watcher.Close()
	}
	if idx.reindexTimer != nil {
		idx.reindexTimer.Stop()
	}
	idx.mu.Unlock()
	return idx.db.Close()
}
