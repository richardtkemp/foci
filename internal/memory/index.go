package memory

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"foci/internal/log"
	"foci/internal/sqlite"

	"github.com/fsnotify/fsnotify"
)

// Result is a single search result from the FTS5 index.
type Result struct {
	Path    string
	Snippet string
	Source  string // source name (e.g., "memory", "code", "docs") or "conversation"
	Rank    float64
	Time    time.Time // message time for conversations, file mtime for memory files (zero if unavailable)
	RowID   int64     // conversation message row ID (0 for non-conversation results)
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

	// Temporal decay (#352): recency boost on relevance-sorted results. Defaults
	// set in NewIndex, overridden from [memory] config via SetTemporalDecay.
	temporalDecay     bool
	decayHalfLifeDays float64
	decayBoost        float64
	evergreenPatterns []string
}

// NewIndex creates or opens an FTS5 index at dbPath, indexing .md files from the given sources.
// sources maps source names to {dir, weight}. debounce is the delay before auto-reindexing
// on file change (0s = immediate). conversationWeight is the multiplier for conversation search results.
func NewIndex(dbPath string, sources map[string]SourceConfig, debounce time.Duration, conversationWeight float64) (*Index, error) {
	db, err := sqlite.OpenInit(dbPath,
		`CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
			content, path, source,
			tokenize='porter unicode61'
		)`,
		`CREATE TABLE IF NOT EXISTS memory_meta (
			source TEXT NOT NULL,
			path TEXT NOT NULL,
			mtime REAL NOT NULL,
			PRIMARY KEY (source, path)
		)`,
	)
	if err != nil {
		return nil, err
	}

	return &Index{
		db:                 db,
		sources:            sources,
		conversationWeight: conversationWeight,
		debounce:           debounce,
		// #352 defaults (overridable via SetTemporalDecay).
		temporalDecay:     true,
		decayHalfLifeDays: 10,
		decayBoost:        1.0,
		evergreenPatterns: []string{"MEMORY.md", "research-*"},
	}, nil
}

// SetTemporalDecay overrides the recency-boost settings from [memory] config
// (#352). evergreen may be nil to keep the defaults. Call once after construction.
func (idx *Index) SetTemporalDecay(enabled bool, halfLifeDays, boost float64, evergreen []string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.temporalDecay = enabled
	if halfLifeDays > 0 {
		idx.decayHalfLifeDays = halfLifeDays
	}
	if boost > 0 {
		idx.decayBoost = boost
	}
	if evergreen != nil {
		idx.evergreenPatterns = evergreen
	}
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
		if _, err := os.Stat(sourceCfg.Dir); os.IsNotExist(err) {
			log.Infof("memory", "fts5: skipping source %q: directory %s does not exist yet", sourceName, sourceCfg.Dir)
			continue
		}
		if err := filepath.Walk(sourceCfg.Dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				// A file listed by the dir walk can vanish before we lstat it — e.g. a
				// sqlite -shm/-wal sidecar next to a source DB. Tolerate it (skip) rather
				// than aborting the whole reindex (#1033). info is nil on a walk error.
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".md") {
				return nil
			}

			data, err := os.ReadFile(path)
			if err != nil {
				log.Warnf("memory", "fts5 reindex: skipping unreadable file %s: %v", path, err)
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

// IndexConversation adds a conversation message to the FTS5 index. The rowID
// parameter is accepted for signature parity with BleveIndex but unused — the
// one-time BackfillConversations rebuild (not per-message dedup) handles history.
func (idx *Index) IndexConversation(text, session string, _ int64) {
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

// BackfillConversations does a ONE-TIME rebuild of the conversation portion of
// the FTS5 index from one or more conversation SQLite logs, so historical chat
// is searchable (FTS5 otherwise only indexes conversation in real-time). Pass
// EVERY conversation DB that feeds this index in a single call — in shared mode
// one FTS5 index serves all agents' per-agent conversation DBs, so they must all
// be rebuilt inside the same wipe.
//
// FTS5 has no per-message doc ID, so rather than dedupe it wipes the derived
// conversation rows and re-inserts them all — guarded by a marker table so it
// runs only once. Wipe + re-insert + marker are ONE transaction: a crash rolls
// back to unmarked, so the next startup cleanly re-runs it (never a
// marked-but-half-populated state). Memory FILES are untouched. Returns the
// number of messages indexed.
func (idx *Index) BackfillConversations(convDBPaths ...string) (int, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Already migrated? The marker table's existence is the one-time flag.
	var marked int
	_ = idx.db.QueryRow(
		`SELECT 1 FROM sqlite_master WHERE type='table' AND name='conversation_backfilled'`,
	).Scan(&marked)
	if marked == 1 {
		return 0, nil
	}

	tx, err := idx.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin backfill tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Wipe the derived conversation rows (source='conversation' only; files stay).
	if _, err := tx.Exec(`DELETE FROM memory_fts WHERE source = 'conversation'`); err != nil {
		return 0, fmt.Errorf("wipe conversation fts: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM memory_meta WHERE source = 'conversation'`); err != nil {
		return 0, fmt.Errorf("wipe conversation meta: %w", err)
	}

	insFTS, err := tx.Prepare(`INSERT INTO memory_fts (content, path, source) VALUES (?, ?, 'conversation')`)
	if err != nil {
		return 0, fmt.Errorf("prepare fts insert: %w", err)
	}
	defer func() { _ = insFTS.Close() }()
	upMeta, err := tx.Prepare(`INSERT OR REPLACE INTO memory_meta (source, path, mtime) VALUES ('conversation', ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("prepare meta upsert: %w", err)
	}
	defer func() { _ = upMeta.Close() }()

	var count int
	for _, convDBPath := range convDBPaths {
		if _, err := os.Stat(convDBPath); err != nil {
			continue // no conversation DB for this agent yet — nothing to rebuild
		}
		n, err := backfillOneConvDB(convDBPath, insFTS, upMeta)
		if err != nil {
			return 0, err
		}
		count += n
	}

	// Marker LAST, same tx: commit makes backfill+marker atomic.
	if _, err := tx.Exec(`CREATE TABLE conversation_backfilled (done INTEGER)`); err != nil {
		return 0, fmt.Errorf("create marker: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit backfill: %w", err)
	}
	committed = true
	return count, nil
}

// backfillOneConvDB reads every non-empty message from a conversation SQLite log
// and inserts it into the FTS5 index via the prepared statements (which belong
// to the caller's transaction). Returns the number inserted.
func backfillOneConvDB(convDBPath string, insFTS, upMeta *sql.Stmt) (int, error) {
	conv, err := sqlite.Open(convDBPath)
	if err != nil {
		return 0, fmt.Errorf("open conversation db %s: %w", convDBPath, err)
	}
	defer func() { _ = conv.Close() }()

	rows, err := conv.Query(`SELECT ts, text, session FROM messages WHERE text != '' ORDER BY id`)
	if err != nil {
		return 0, fmt.Errorf("query conversation messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var count int
	for rows.Next() {
		var ts, text string
		var session sql.NullString
		if err := rows.Scan(&ts, &text, &session); err != nil {
			return 0, fmt.Errorf("scan conversation row: %w", err)
		}
		sess := session.String
		if _, err := insFTS.Exec(text, sess); err != nil {
			return 0, fmt.Errorf("insert conversation fts: %w", err)
		}
		mtime := float64(time.Now().Unix())
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			mtime = float64(t.Unix())
		}
		if _, err := upMeta.Exec(sess, mtime); err != nil {
			return 0, fmt.Errorf("upsert conversation meta: %w", err)
		}
		count++
	}
	return count, rows.Err()
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

	return "CASE f.source " + strings.Join(cases, " ") + " END"
}

// sanitizeFTS5Query wraps each space-separated term in double quotes to prevent
// FTS5 from interpreting special characters as query operators. Without this,
// hyphens trigger column-filter parsing (e.g. "hunter-alpha" → column "alpha"),
// and words like OR/AND/NOT/NEAR are treated as boolean operators.
func sanitizeFTS5Query(query string) string {
	terms := strings.Fields(query)
	if len(terms) == 0 {
		return query
	}
	for i, t := range terms {
		t = strings.ReplaceAll(t, `"`, `""`)
		terms[i] = `"` + t + `"`
	}
	return strings.Join(terms, " ")
}

// Search queries the FTS5 index. sort controls result ordering:
// "relevance" (default/empty) orders by weighted rank,
// "newest" orders by file mtime descending, "oldest" orders by mtime ascending.
// opts provides optional date range filtering.
func (idx *Index) Search(query string, sort string, opts *SearchOptions) ([]Result, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var extraFilter string
	var args []interface{}
	args = append(args, sanitizeFTS5Query(query))

	if opts != nil {
		if opts.ExcludePath != "" {
			extraFilter += " AND f.path != ?"
			args = append(args, opts.ExcludePath)
		}
		if opts.DateFrom != nil {
			extraFilter += " AND m.mtime >= ?"
			args = append(args, float64(opts.DateFrom.Unix()))
		}
		if opts.DateTo != nil {
			extraFilter += " AND m.mtime < ?"
			args = append(args, float64(opts.DateTo.Unix()))
		}
	}

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
			       COALESCE(m.mtime, 0) AS sort_mtime,
			       COALESCE(m.mtime, 0)
			FROM memory_fts f
			LEFT JOIN memory_meta m ON f.source = m.source AND f.path = m.path
			WHERE memory_fts MATCH ?%s
			ORDER BY sort_mtime %s
			LIMIT 20
		`, extraFilter, order)
	default:
		weightedRankCase := idx.buildWeightedRankCase()
		sqlStr = fmt.Sprintf(`
			SELECT f.path,
			       snippet(memory_fts, 0, '>', '<', '...', 40),
			       f.source,
			       %s AS weighted_rank,
			       COALESCE(m.mtime, 0)
			FROM memory_fts f
			LEFT JOIN memory_meta m ON f.source = m.source AND f.path = m.path
			WHERE memory_fts MATCH ?%s
			ORDER BY weighted_rank
			LIMIT 40
		`, weightedRankCase, extraFilter)
	}

	rows, err := idx.db.Query(sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []Result
	for rows.Next() {
		var r Result
		var mtime float64
		if err := rows.Scan(&r.Path, &r.Snippet, &r.Source, &r.Rank, &mtime); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if mtime > 0 {
			r.Time = time.Unix(int64(mtime), 0)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Recency boost for relevance sort (#352). FTS5 weighted_rank is negative
	// (more-negative = better) → higherIsBetter=false. When decay is off, hl/boost
	// are 0 so this degenerates to a plain rank-ascending sort + truncate to the
	// return limit — same top-N as before the over-fetch.
	if sort == "" || sort == "relevance" {
		hl, boost := 0.0, 0.0
		if idx.temporalDecay {
			hl, boost = idx.decayHalfLifeDays, idx.decayBoost
		}
		results = rerankByRecency(results, time.Now(), hl, boost, idx.evergreenPatterns, memorySearchReturn, false)
	}
	return results, nil
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

	if err := watchSources(watcher, idx.sources); err != nil {
		return err
	}
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
