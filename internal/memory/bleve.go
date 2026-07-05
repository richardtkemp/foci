package memory

import (
	"database/sql"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"foci/internal/log"
	"foci/internal/sqlite"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search"
	blevehtml "github.com/blevesearch/bleve/v2/search/highlight/format/html"
	"github.com/blevesearch/bleve/v2/search/query"
	"github.com/fsnotify/fsnotify"
)

// BleveIndex manages a bleve full-text search index over memory files
// and conversation history. Multiple sources can be indexed, each
// with a configurable weight multiplier.
type BleveIndex struct {
	indexPath          string
	index              bleve.Index
	sources            map[string]SourceConfig
	conversationWeight float64 // weight multiplier for conversation results
	watcher            *fsnotify.Watcher
	debounce           time.Duration
	reindexTimer       *time.Timer
	sweepStop          chan struct{}
	closed             bool
	mu                 sync.Mutex
}

// buildBleveMapping creates the index mapping for search documents.
// Supports memory files, conversation history, and todo items.
func buildBleveMapping() mapping.IndexMapping {
	contentField := bleve.NewTextFieldMapping()
	contentField.Store = true
	contentField.IncludeInAll = true
	contentField.IncludeTermVectors = true // needed for highlighting

	pathField := bleve.NewKeywordFieldMapping()
	pathField.Store = true
	pathField.IncludeInAll = false

	sourceField := bleve.NewKeywordFieldMapping()
	sourceField.Store = true
	sourceField.IncludeInAll = false

	mtimeField := bleve.NewNumericFieldMapping()
	mtimeField.Store = true
	mtimeField.IncludeInAll = false

	// agent_id is used for per-agent filtering (e.g. todo search).
	// Empty for memory/conversation docs.
	agentIDField := bleve.NewKeywordFieldMapping()
	agentIDField.Store = true
	agentIDField.IncludeInAll = false

	// todo_id stores the per-agent todo ID for result lookup.
	todoIDField := bleve.NewNumericFieldMapping()
	todoIDField.Store = true
	todoIDField.IncludeInAll = false

	docMapping := bleve.NewDocumentMapping()
	docMapping.AddFieldMappingsAt("content", contentField)
	docMapping.AddFieldMappingsAt("path", pathField)
	docMapping.AddFieldMappingsAt("source", sourceField)
	docMapping.AddFieldMappingsAt("mtime", mtimeField)
	docMapping.AddFieldMappingsAt("agent_id", agentIDField)
	docMapping.AddFieldMappingsAt("todo_id", todoIDField)

	indexMapping := bleve.NewIndexMapping()
	indexMapping.DefaultMapping = docMapping
	indexMapping.DefaultAnalyzer = "en" // English analyzer with Porter stemming
	return indexMapping
}

// NewBleveIndex creates or opens a bleve index at indexPath, indexing .md files
// from the given sources. debounce is the delay before auto-reindexing on file change.
// conversationWeight is the multiplier for conversation search results.
func NewBleveIndex(indexPath string, sources map[string]SourceConfig, debounce time.Duration, conversationWeight float64) (*BleveIndex, error) {
	var idx bleve.Index
	var err error

	idx, err = bleve.Open(indexPath)
	if err != nil {
		// Index doesn't exist or is corrupt — create fresh
		_ = os.RemoveAll(indexPath) // clean up any partial index
		idx, err = bleve.New(indexPath, buildBleveMapping())
	}
	if err != nil {
		return nil, fmt.Errorf("open bleve index: %w", err)
	}

	return &BleveIndex{
		indexPath:          indexPath,
		index:              idx,
		sources:            sources,
		conversationWeight: conversationWeight,
		debounce:           debounce,
	}, nil
}

// Reindex refreshes the FILE portion of the index in place: it re-reads the
// configured source directories, upserts each .md file's doc, and prunes docs
// for files that no longer exist. Conversation and todo docs are write-once
// (added incrementally via IndexConversation / todo sync) and are deliberately
// left untouched — the index is NEVER recreated, so the (large) conversation
// corpus is not round-tripped through memory. Peak memory therefore scales with
// the file corpus, not the conversation history. Mirrors the FTS5 backend's
// delete-file-rows-then-reinsert approach. Instrumented with RSS/heap logging.
func (b *BleveIndex) Reindex() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	start := time.Now()
	rssStart := readSelfRSSkB()
	log.Infof("memory", "bleve reindex START %s: rss=%dMB heap=%dMB", b.indexPath, rssStart/1024, heapAllocMB())

	// Upsert current .md files into a batch.
	batch := b.index.NewBatch()
	current := make(map[string]bool)
	var fileCount int
	var fileBytes int64
	for sourceName, sourceCfg := range b.sources {
		if _, err := os.Stat(sourceCfg.Dir); os.IsNotExist(err) {
			log.Infof("memory", "bleve: skipping source %q: directory %s does not exist yet", sourceName, sourceCfg.Dir)
			continue
		}
		var srcFiles int
		var srcBytes int64
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
				log.Warnf("memory", "bleve reindex: skipping unreadable file %s: %v", path, err)
				return nil // skip unreadable
			}
			if len(data) == 0 {
				return nil
			}
			srcFiles++
			srcBytes += int64(len(data))

			relPath, _ := filepath.Rel(sourceCfg.Dir, path)
			docID := sourceName + ":" + relPath
			current[docID] = true
			doc := map[string]interface{}{
				"content": html.UnescapeString(string(data)),
				"path":    relPath,
				"source":  sourceName,
				"mtime":   float64(info.ModTime().Unix()),
			}
			if err := batch.Index(docID, doc); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return fmt.Errorf("index source %q: %w", sourceName, err)
		}
		fileCount += srcFiles
		fileBytes += srcBytes
		log.Infof("memory", "bleve reindex %s: source %q = %d files, %dKB", b.indexPath, sourceName, srcFiles, srcBytes/1024)
	}

	// Prune docs for files that no longer exist on disk. IDs only — no stored
	// content is loaded — so this stays cheap regardless of corpus size, and it
	// never reads conversation/todo docs.
	var deleted int
	for _, id := range b.collectFileDocIDs() {
		if !current[id] {
			batch.Delete(id)
			deleted++
		}
	}

	err := b.index.Batch(batch)
	rssDone := readSelfRSSkB()
	log.Infof("memory", "bleve reindex DONE %s in %s: %d file docs (%dKB) upserted, %d stale pruned; conv/todo docs untouched; rss=%dMB (Δ+%dMB) heap=%dMB err=%v",
		b.indexPath, time.Since(start).Round(time.Millisecond), fileCount, fileBytes/1024, deleted,
		rssDone/1024, (rssDone-rssStart)/1024, heapAllocMB(), err)
	return err
}

// readSelfRSSkB returns this process's resident set size in kB (0 on error).
// Mirrors readSelfRSS in internal/resources/memory_guard.go.
func readSelfRSSkB() int64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				v, _ := strconv.ParseInt(f[1], 10, 64)
				return v
			}
		}
	}
	return 0
}

// heapAllocMB returns the current Go heap allocation in MB.
func heapAllocMB() uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc / 1024 / 1024
}

// collectFileDocIDs returns the IDs of all docs indexed under a configured
// file source (i.e. not conversation/todo). It requests IDs only — no stored
// fields are loaded — so it stays cheap regardless of corpus size and never
// materialises conversation content. Used by Reindex to prune docs whose
// backing file was deleted. Must be called with b.mu held.
func (b *BleveIndex) collectFileDocIDs() []string {
	if len(b.sources) == 0 {
		return nil
	}
	qs := make([]query.Query, 0, len(b.sources))
	for name := range b.sources {
		tq := query.NewTermQuery(name)
		tq.SetField("source")
		qs = append(qs, tq)
	}
	req := bleve.NewSearchRequest(bleve.NewDisjunctionQuery(qs...))
	req.Size = 1_000_000 // IDs only; the file corpus is small

	result, err := b.index.Search(req)
	if err != nil {
		log.Warnf("memory", "collect file doc IDs for reindex: %v", err)
		return nil
	}

	ids := make([]string, 0, len(result.Hits))
	for _, hit := range result.Hits {
		ids = append(ids, hit.ID)
	}
	return ids
}

// IndexConversation adds a conversation message to the bleve index.
// IndexConversation indexes a conversation message. The rowID should be the
// SQLite row ID from the conversation log — this ensures the doc ID matches
// what BackfillConversations uses, preventing duplicates.
func (b *BleveIndex) IndexConversation(text, session string, rowID int64) {
	if text == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	// Use conversation:{session}:{rowID} — same scheme as BackfillConversations.
	docID := fmt.Sprintf("conversation:%s:%d", session, rowID)
	doc := map[string]interface{}{
		"content": text,
		"path":    session,
		"source":  "conversation",
		"mtime":   float64(time.Now().Unix()),
	}
	if err := b.index.Index(docID, doc); err != nil {
		log.Errorf("memory", "bleve index conversation: %v", err)
	}
}

// BackfillConversations reads historical messages from a conversation SQLite
// database and indexes any that are missing from the bleve index. Returns the
// number of messages backfilled. Safe to call concurrently with other index
// operations.
func (b *BleveIndex) BackfillConversations(dbPath string) (int, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return 0, nil // no conversation DB yet — nothing to backfill
	}

	db, err := sqlite.Open(dbPath)
	if err != nil {
		return 0, fmt.Errorf("open conversation db: %w", err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query(
		`SELECT id, ts, text, session FROM messages WHERE text != '' ORDER BY id`,
	)
	if err != nil {
		return 0, fmt.Errorf("query conversation messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	const backfillBatchSize = 1000

	var count int
	batch := b.index.NewBatch()
	batchCount := 0

	for rows.Next() {
		var id int64
		var ts, text string
		var session sql.NullString
		if err := rows.Scan(&id, &ts, &text, &session); err != nil {
			return count, fmt.Errorf("scan row: %w", err)
		}

		sess := session.String
		docID := fmt.Sprintf("conversation:%s:%d", sess, id)

		// Check if already indexed
		existing, _ := b.index.Document(docID)
		if existing != nil {
			continue
		}

		mtime := float64(time.Now().Unix())
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			mtime = float64(t.Unix())
		}

		doc := map[string]interface{}{
			"content": text,
			"path":    sess,
			"source":  "conversation",
			"mtime":   mtime,
		}
		if err := batch.Index(docID, doc); err != nil {
			log.Errorf("memory", "bleve backfill index doc: %v", err)
			continue
		}
		count++
		batchCount++

		if batchCount >= backfillBatchSize {
			b.mu.Lock()
			err = b.index.Batch(batch)
			b.mu.Unlock()
			if err != nil {
				return count - batchCount, fmt.Errorf("commit backfill batch: %w", err)
			}
			batch = b.index.NewBatch()
			batchCount = 0
		}
	}
	if err := rows.Err(); err != nil {
		return count, fmt.Errorf("iterate rows: %w", err)
	}

	if batchCount > 0 {
		b.mu.Lock()
		err = b.index.Batch(batch)
		b.mu.Unlock()
		if err != nil {
			return count - batchCount, fmt.Errorf("commit backfill batch: %w", err)
		}
	}

	return count, nil
}

// TodoSearchHit is a single todo search result from bleve, carrying the
// per-agent todo ID and relevance rank for subsequent SQLite lookup.
type TodoSearchHit struct {
	TodoID int64
	Rank   float64
}

// todoDocID returns the bleve document ID for a todo item.
func todoDocID(agentID string, id int64) string {
	return fmt.Sprintf("todo:%s:%d", agentID, id)
}

// IndexTodo adds or updates a todo item in the bleve index.
func (b *BleveIndex) IndexTodo(agentID string, id int64, text string, mtime float64) {
	if text == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	doc := map[string]interface{}{
		"content":  text,
		"path":     fmt.Sprintf("#%d", id),
		"source":   "todo",
		"mtime":    mtime,
		"agent_id": agentID,
		"todo_id":  float64(id),
	}
	if err := b.index.Index(todoDocID(agentID, id), doc); err != nil {
		log.Errorf("memory", "bleve index todo: %v", err)
	}
}

// RemoveTodo removes a todo item from the bleve index.
func (b *BleveIndex) RemoveTodo(agentID string, id int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.index.Delete(todoDocID(agentID, id)); err != nil {
		log.Errorf("memory", "bleve remove todo: %v", err)
	}
}

// SearchTodos queries the bleve index for todo items matching the query,
// filtered by agent ID. Returns hits with todo IDs and relevance ranks.
//
// sortOrder controls ordering: "" or "relevance" sorts by score (default),
// "newest" by mtime descending, "oldest" by mtime ascending.
// limit caps the number of results (0 uses default of 10).
func (b *BleveIndex) SearchTodos(agentID, queryStr, sortOrder string, limit int) ([]TodoSearchHit, error) {
	if strings.TrimSpace(queryStr) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	// Build the text query. If all terms are negated (e.g. "-android"),
	// bleve's QueryStringQuery returns nothing because there are no positive
	// terms to match against. Detect this and use MatchAll + BooleanQuery
	// must-not instead.
	var textQuery query.Query
	if neg, terms := allNegatedTerms(queryStr); neg {
		bq := bleve.NewBooleanQuery()
		bq.AddMust(bleve.NewMatchAllQuery())
		for _, term := range terms {
			mq := bleve.NewMatchQuery(term)
			mq.SetField("content")
			bq.AddMustNot(mq)
		}
		textQuery = bq
	} else {
		textQuery = bleve.NewQueryStringQuery(sanitizeBleveQuery(queryStr))
	}

	// Filter: source = "todo"
	sourceQuery := query.NewTermQuery("todo")
	sourceQuery.SetField("source")

	// Filter: agent_id = agentID
	agentQuery := query.NewTermQuery(agentID)
	agentQuery.SetField("agent_id")

	combined := bleve.NewConjunctionQuery(textQuery, sourceQuery, agentQuery)

	req := bleve.NewSearchRequest(combined)
	req.Size = limit
	req.Fields = []string{"todo_id"}

	switch sortOrder {
	case "created", "updated":
		req.SortBy([]string{"-mtime"})
	case "created_asc", "updated_asc":
		req.SortBy([]string{"mtime"})
	default:
		req.SortBy([]string{"-_score"})
	}

	result, err := b.index.Search(req)
	if err != nil {
		return nil, fmt.Errorf("bleve todo search: %w", err)
	}

	hits := make([]TodoSearchHit, 0, len(result.Hits))
	for _, hit := range result.Hits {
		todoID, _ := hit.Fields["todo_id"].(float64)
		hits = append(hits, TodoSearchHit{
			TodoID: int64(todoID),
			Rank:   hit.Score,
		})
	}
	return hits, nil
}

// allNegatedTerms checks whether a query string consists entirely of negated
// terms (e.g. "-android", "-foo -bar"). Returns true and the bare terms
// (without "-") if so. Mixed queries like "deploy -android" return false.
func allNegatedTerms(q string) (bool, []string) {
	fields := strings.Fields(q)
	if len(fields) == 0 {
		return false, nil
	}
	var terms []string
	for _, f := range fields {
		if !strings.HasPrefix(f, "-") || len(f) < 2 {
			return false, nil
		}
		terms = append(terms, f[1:])
	}
	return true, terms
}

// sanitizeBleveQuery wraps each space-separated term in double quotes to prevent
// Bleve's QueryStringQuery from interpreting special characters as operators.
// Without this, hyphens are treated as must-not (e.g. "hunter-alpha" excludes
// "alpha"), and characters like +, :, ^, ~, * have special meaning.
func sanitizeBleveQuery(q string) string {
	terms := strings.Fields(q)
	if len(terms) == 0 {
		return q
	}
	for i, t := range terms {
		t = strings.ReplaceAll(t, `\`, `\\`)
		t = strings.ReplaceAll(t, `"`, `\"`)
		terms[i] = `"` + t + `"`
	}
	return strings.Join(terms, " ")
}

// Search queries the bleve index. sort controls result ordering:
// "relevance" (default/empty) orders by weighted score,
// "newest" orders by mtime descending, "oldest" orders by mtime ascending.
// opts provides optional date range filtering.
func (b *BleveIndex) Search(queryStr string, sortOrder string, opts *SearchOptions) ([]Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Build the main query
	mainQuery := bleve.NewQueryStringQuery(sanitizeBleveQuery(queryStr))

	var finalQuery query.Query = mainQuery

	// Apply filters if provided
	if opts != nil {
		var mustClauses []query.Query
		var mustNotClauses []query.Query

		if opts.ExcludePath != "" {
			eq := query.NewTermQuery(opts.ExcludePath)
			eq.SetField("path")
			mustNotClauses = append(mustNotClauses, eq)
		}

		if opts.DateFrom != nil || opts.DateTo != nil {
			minInclusive := boolPtr(true)
			maxInclusive := boolPtr(opts.DateTo == nil)
			dateQuery := query.NewNumericRangeInclusiveQuery(
				floatPtrFromTime(opts.DateFrom),
				floatPtrFromTime(opts.DateTo),
				minInclusive,
				maxInclusive,
			)
			dateQuery.SetField("mtime")
			mustClauses = append(mustClauses, dateQuery)
		}

		if len(mustClauses) > 0 || len(mustNotClauses) > 0 {
			bq := bleve.NewBooleanQuery()
			bq.AddMust(mainQuery)
			for _, c := range mustClauses {
				bq.AddMust(c)
			}
			for _, c := range mustNotClauses {
				bq.AddMustNot(c)
			}
			finalQuery = bq
		}
	}

	req := bleve.NewSearchRequest(finalQuery)
	req.Size = 20
	req.Fields = []string{"path", "source", "mtime"}

	// Highlight content field for snippets
	req.Highlight = bleve.NewHighlightWithStyle(blevehtml.Name)
	req.Highlight.AddField("content")

	switch sortOrder {
	case "newest":
		req.SortBy([]string{"-mtime"})
	case "oldest":
		req.SortBy([]string{"mtime"})
	default:
		// relevance — bleve sorts by score by default
		req.SortBy([]string{"-_score"})
	}

	searchResult, err := b.index.Search(req)
	if err != nil {
		return nil, fmt.Errorf("bleve search: %w", err)
	}

	results := make([]Result, 0, len(searchResult.Hits))
	for _, hit := range searchResult.Hits {
		path, _ := hit.Fields["path"].(string)
		source, _ := hit.Fields["source"].(string)

		snippet := buildSnippet(hit)

		rank := hit.Score
		// Apply source weight multiplier (same formula as FTS5)
		if source == "conversation" {
			rank *= b.conversationWeight
		} else if cfg, ok := b.sources[source]; ok {
			rank *= (1.0 + cfg.Weight)
		}

		r := Result{
			Path:    path,
			Snippet: snippet,
			Source:  source,
			Rank:    rank,
		}
		if mtime, ok := hit.Fields["mtime"].(float64); ok && mtime > 0 {
			r.Time = time.Unix(int64(mtime), 0)
		}
		// Parse row ID from bleve doc ID: "conversation:{session}:{rowID}"
		if source == "conversation" {
			if trimmed := strings.TrimPrefix(hit.ID, "conversation:"); trimmed != hit.ID {
				if lastColon := strings.LastIndex(trimmed, ":"); lastColon > 0 {
					if id, err := strconv.ParseInt(trimmed[lastColon+1:], 10, 64); err == nil {
						r.RowID = id
					}
				}
			}
		}
		results = append(results, r)
	}

	// For relevance sort, re-sort by weighted rank (descending — higher is better)
	if sortOrder == "" || sortOrder == "relevance" {
		sort.Slice(results, func(i, j int) bool {
			return results[i].Rank > results[j].Rank
		})
	}

	return results, nil
}

// floatPtrFromTime converts a time pointer to a float64 pointer (unix timestamp).
func floatPtrFromTime(t *time.Time) *float64 {
	if t == nil {
		return nil
	}
	f := float64(t.Unix())
	return &f
}

func boolPtr(v bool) *bool { return &v }

// buildSnippet extracts a snippet from a bleve search hit.
// Prefers highlighted fragments; falls back to the first ~200 chars of content.
func buildSnippet(hit *search.DocumentMatch) string {
	if frags, ok := hit.Fragments["content"]; ok && len(frags) > 0 {
		// Replace HTML highlight markers with > < (matching FTS5 style)
		s := frags[0]
		s = strings.ReplaceAll(s, "<mark>", ">")
		s = strings.ReplaceAll(s, "</mark>", "<")
		return html.UnescapeString(s)
	}
	return ""
}

// Watch starts file system watching on all source directories.
func (b *BleveIndex) Watch() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}

	b.mu.Lock()
	b.watcher = watcher
	b.mu.Unlock()

	if err := watchSources(watcher, b.sources); err != nil {
		return err
	}

	go b.handleFileEvents()
	return nil
}

// handleFileEvents processes file system events from the watcher.
func (b *BleveIndex) handleFileEvents() {
	for {
		select {
		case event, ok := <-b.watcher.Events:
			if !ok {
				return
			}
			if filepath.Ext(event.Name) == ".md" {
				b.scheduleReindex()
			}
		case err, ok := <-b.watcher.Errors:
			if !ok {
				return
			}
			log.Warnf("memory", "bleve file watcher error: %v", err)
		}
	}
}

// scheduleReindex debounces reindex requests.
func (b *BleveIndex) scheduleReindex() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.reindexTimer != nil {
		b.reindexTimer.Stop()
	}

	b.reindexTimer = time.AfterFunc(b.debounce, func() {
		if err := b.Reindex(); err != nil {
			log.Errorf("memory", "bleve auto-reindex failed: %v", err)
		}
	})
}

// StartSweep launches a background goroutine that calls Reindex periodically.
func (b *BleveIndex) StartSweep(initial, interval time.Duration) {
	b.mu.Lock()
	b.sweepStop = make(chan struct{})
	stop := b.sweepStop
	b.mu.Unlock()

	go runSweepLoop(stop, initial, interval, "bleve sweep", b.Reindex)
}

// Close closes the watcher, stops the sweep goroutine, and closes the index.
func (b *BleveIndex) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	if b.sweepStop != nil {
		close(b.sweepStop)
		b.sweepStop = nil
	}
	if b.watcher != nil {
		_ = b.watcher.Close()
	}
	if b.reindexTimer != nil {
		b.reindexTimer.Stop()
	}
	b.mu.Unlock()
	return b.index.Close()
}
