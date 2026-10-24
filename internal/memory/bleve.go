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
	"foci/internal/procinfo"
	"foci/internal/sqlite"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search"
	blevehtml "github.com/blevesearch/bleve/v2/search/highlight/format/html"
	"github.com/blevesearch/bleve/v2/search/query"
	index "github.com/blevesearch/bleve_index_api"
	"github.com/fsnotify/fsnotify"
)

var (
	memoryLog = log.NewComponentLogger("memory")
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

	// Temporal decay (#352): recency boost applied to relevance-sorted results.
	// Defaults set in NewBleveIndex; overridden from [memory] config via
	// SetTemporalDecay. Read under mu inside Search.
	searchLimit       int // results per Search ([memory] search_limit; 0 = default)
	temporalDecay     bool
	decayHalfLifeDays float64
	decayBoost        float64
	evergreenPatterns []string
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
	// BM25, not bleve's default TF-IDF (Dick, 2026-10-07): term frequency
	// saturates and length is judged against the average document, so a long
	// memory file is not buried under short chat messages that match one word
	// (#2234). The model is stored in the index; see openOrRebuildBleve.
	indexMapping.ScoringModel = index.BM25Scoring
	return indexMapping
}

// BM25 with NO length normalisation (b = 0; bleve's default is 0.75).
//
// One index holds tens of thousands of chat messages a few words long and
// daily memory files thousands of words long. Normalising by length against
// that mixed average makes a memory file hundreds of times "too long", and a
// one-word chat match outranks a file containing every query word — measured
// on clutch's live data 2026-10-07: with b=0.75 (and with b=0.3) "todo chart"
// returned no memory file in the top 20; with b=0 the three files that name
// the todo-chart tool ranked 1-3 (#2234). k1 saturation still stops a file
// that repeats one word from dominating.
//
// bleve exposes b only as this package-level variable, so it applies to every
// bleve index in the process; all of them are built by this file.
func init() { search.BM25_b = 0 }

// openOrRebuildBleve opens the index at indexPath, or creates it fresh when it
// is missing, corrupt, or was built with a different scoring model than
// buildBleveMapping now sets. bleve keeps the mapping from creation time, so a
// mapping change only reaches an existing index by rebuilding it. Dropping the
// docs is safe: startup re-fills memory files (Reindex), conversations
// (BackfillConversations) and todos (IndexAllTodos).
func openOrRebuildBleve(indexPath string) (bleve.Index, error) {
	want := buildBleveMapping()
	idx, err := bleve.Open(indexPath)
	if err == nil {
		im, ok := idx.Mapping().(*mapping.IndexMappingImpl)
		if ok && im.ScoringModel == want.(*mapping.IndexMappingImpl).ScoringModel {
			return idx, nil
		}
		got := "?"
		if ok {
			got = im.ScoringModel
		}
		memoryLog.Infof("bleve index %s uses scoring model %q; rebuilding for %q", indexPath, got, index.BM25Scoring)
		_ = idx.Close()
	}
	// Missing, corrupt or outdated — create fresh.
	_ = os.RemoveAll(indexPath) // clean up any partial or outdated index
	return bleve.New(indexPath, want)
}

// NewBleveIndex creates or opens a bleve index at indexPath, indexing .md files
// from the given sources. debounce is the delay before auto-reindexing on file change.
// conversationWeight is the multiplier for conversation search results.
func NewBleveIndex(indexPath string, sources map[string]SourceConfig, debounce time.Duration, conversationWeight float64) (*BleveIndex, error) {
	idx, err := openOrRebuildBleve(indexPath)
	if err != nil {
		return nil, fmt.Errorf("open bleve index: %w", err)
	}

	return &BleveIndex{
		indexPath:          indexPath,
		index:              idx,
		sources:            sources,
		conversationWeight: conversationWeight,
		debounce:           debounce,
		// #352 defaults (overridable via SetTemporalDecay): on, 10-day half-life,
		// up to 2x boost for brand-new results, timeless files exempt.
		temporalDecay:     true,
		decayHalfLifeDays: 10,
		decayBoost:        1.0,
		evergreenPatterns: []string{"MEMORY.md", "research-*"},
	}, nil
}

// SetTemporalDecay overrides the recency-boost settings from [memory] config
// (#352). evergreen may be nil to keep the default patterns. Safe to call once
// right after construction, before the index is used.
// SetSearchLimit overrides how many results Search returns ([memory] search_limit);
// n <= 0 keeps the default. Call once after construction.
func (b *BleveIndex) SetSearchLimit(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > 0 {
		b.searchLimit = n
	}
}

// searchLimitLocked reads the limit; callers already hold b.mu (Search locks it).
func (b *BleveIndex) searchLimitLocked() int {
	if b.searchLimit > 0 {
		return b.searchLimit
	}
	return memorySearchReturn
}

func (b *BleveIndex) SetTemporalDecay(enabled bool, halfLifeDays, boost float64, evergreen []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.temporalDecay = enabled
	if halfLifeDays > 0 {
		b.decayHalfLifeDays = halfLifeDays
	}
	if boost > 0 {
		b.decayBoost = boost
	}
	if evergreen != nil {
		b.evergreenPatterns = evergreen
	}
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
	memoryLog.Infof("bleve reindex START %s: rss=%dMB heap=%dMB", b.indexPath, rssStart/1024, heapAllocMB())

	// Upsert current .md files into a batch.
	batch := b.index.NewBatch()
	current := make(map[string]bool)
	var fileCount int
	var fileBytes int64
	for sourceName, sourceCfg := range b.sources {
		if _, err := os.Stat(sourceCfg.Dir); os.IsNotExist(err) {
			memoryLog.Infof("bleve: skipping source %q: directory %s does not exist yet", sourceName, sourceCfg.Dir)
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
				memoryLog.Warnf("bleve reindex: skipping unreadable file %s: %v", path, err)
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
		memoryLog.Infof("bleve reindex %s: source %q = %d files, %dKB", b.indexPath, sourceName, srcFiles, srcBytes/1024)
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
	memoryLog.Infof("bleve reindex DONE %s in %s: %d file docs (%dKB) upserted, %d stale pruned; conv/todo docs untouched; rss=%dMB (Δ+%dMB) heap=%dMB err=%v",
		b.indexPath, time.Since(start).Round(time.Millisecond), fileCount, fileBytes/1024, deleted,
		rssDone/1024, (rssDone-rssStart)/1024, heapAllocMB(), err)
	return err
}

// readSelfRSSkB returns this process's resident set size in kB (0 on error).
func readSelfRSSkB() int64 {
	v, err := procinfo.RSSKB(os.Getpid())
	if err != nil {
		return 0
	}
	return v
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
		memoryLog.Warnf("collect file doc IDs for reindex: %v", err)
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
		memoryLog.Errorf("bleve index conversation: %v", err)
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
			memoryLog.Errorf("bleve backfill index doc: %v", err)
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
		memoryLog.Errorf("bleve index todo: %v", err)
	}
}

// RemoveTodo removes a todo item from the bleve index.
func (b *BleveIndex) RemoveTodo(agentID string, id int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.index.Delete(todoDocID(agentID, id)); err != nil {
		memoryLog.Errorf("bleve remove todo: %v", err)
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

// phraseBoost is the mild rank boost (#2327) a relevance-sorted multi-word hit
// gets for carrying the query's words next to each other: after the top-N cut,
// Search scales each returned hit's weighted rank by 1 + phraseBoost ×
// (adjacent query word pairs found as a phrase, in order, in the hit's
// content / pairs counted). Relevance sort only — newest/oldest, one-word
// queries and SearchTodos never boost — and it is applied to the hits the
// unboosted search already chose to return, never inside the bleve query and
// never before the cut, so it never changes which hits are returned: it can
// only reorder them. Dick, 2026-10-11:
// "I want a mild phrase boost: on the lower end of the reasonable range."
//
// Size, from an A/B on a copy of clutch's live index (2026-10-11: 98,368
// conversation messages, 340 memory files, 2,304 todos) with 46 real
// multi-word queries from clutch's own foci_memory_search history, boost range
// 0.05-1.0 tried: pair-matching hits in the top 5 rose 104 (no boost) -> 115
// at 0.1 -> 140 at 1.0. Those runs applied the factor before the final
// truncation, which also let a phrase hit from outside the top N take one of
// the last places at the cut (baseline top-20 hits kept fell 920 -> 909 at
// 0.1 -> 886 at 1.0); the shipped after-the-cut application gives up that
// promotion so the returned set stays exactly the unboosted one. 0.1 (a full
// phrase match is worth +10%) gets most of the low-boost gain — the mild,
// low-end size.
//
// Boosting inside the bleve query instead (a phrase clause added to the OR)
// was measured and rejected: it lifts short conversation messages' raw score
// before the source weights apply, so they fill the 2x over-fetch window and
// push memory files out of the results — 15/31/46 memory-file hits lost from
// the top 20 at 0.1/0.2/0.5 — reintroducing exactly the #2234 failure that
// BM25 at b=0 fixed.
//
// Deliberately not a config key: it is a tuned constant, not a preference.
const phraseBoost = 0.1

// bleveContentField is the indexed text field ("content" in buildBleveMapping)
// that Search highlights for snippets and the phrase boost analyses.
const bleveContentField = "content"

// adjacentPairs returns each neighbouring pair of words joined by one space,
// in query order: ["a", "b", "c"] -> ["a b", "b c"]. Fewer than two words
// yields nil.
func adjacentPairs(words []string) []string {
	if len(words) < 2 {
		return nil
	}
	pairs := make([]string, 0, len(words)-1)
	for i := 0; i+1 < len(words); i++ {
		pairs = append(pairs, words[i]+" "+words[i+1])
	}
	return pairs
}

// contentAnalyzer returns the analyzer the index mapping applies to the
// content field — the same analyzer a MatchPhraseQuery on that field analyses
// with — so the boost counts pairs exactly as the phrase search will match
// them. An error means no analyzer is registered; the caller skips the boost.
// Caller holds b.mu.
func (b *BleveIndex) contentAnalyzer() (analysis.Analyzer, error) {
	im := b.index.Mapping()
	name := im.AnalyzerNameForPath(bleveContentField)
	analyzer := im.AnalyzerNamed(name)
	if analyzer == nil {
		return nil, fmt.Errorf("no analyzer registered as %q for field %q", name, bleveContentField)
	}
	return analyzer, nil
}

// phraseMatchIDs returns which of candidateIDs have pair as an exact adjacent
// phrase, in order, in their content. One bleve search restricted to the
// candidate IDs: it cannot match anything outside them and inherits any
// filters the main query already applied. No stored fields, no highlighting.
// candidateIDs must be non-empty (phraseShares checks). Caller holds b.mu.
func (b *BleveIndex) phraseMatchIDs(pair string, candidateIDs []string) (map[string]bool, error) {
	phrase := bleve.NewMatchPhraseQuery(pair)
	phrase.SetField(bleveContentField)
	req := bleve.NewSearchRequest(bleve.NewConjunctionQuery(
		bleve.NewDocIDQuery(candidateIDs),
		phrase,
	))
	req.Size = len(candidateIDs)
	res, err := b.index.Search(req)
	if err != nil {
		return nil, fmt.Errorf("phrase search %q: %w", pair, err)
	}
	matched := make(map[string]bool, len(res.Hits))
	for _, hit := range res.Hits {
		matched[hit.ID] = true
	}
	return matched, nil
}

// phraseShares returns, per document ID, the share of the query's counted
// adjacent word pairs that appear as an exact phrase in that document's
// content; documents matching no pair are absent from the map (share 0). A
// pair counts only when the content field's analyzer reduces it to two or more
// terms: "the chart" analyses to the single term "chart", and a one-term
// phrase would match every document containing it, so it counts neither as
// matched nor in the total. Returns nil, nil — no boost, no extra searching —
// for a query with fewer than two words, no documents, or no counted pair.
// Caller holds b.mu.
func (b *BleveIndex) phraseShares(queryStr string, docIDs []string) (map[string]float64, error) {
	// strings.Fields: the same word split sanitizeBleveQuery quotes, so "word"
	// means the same thing in the main query and the phrase pairs.
	words := strings.Fields(queryStr)
	if len(words) < 2 || len(docIDs) == 0 {
		return nil, nil
	}
	analyzer, err := b.contentAnalyzer()
	if err != nil {
		return nil, err
	}
	var counted []string
	for _, pair := range adjacentPairs(words) {
		if len(analyzer.Analyze([]byte(pair))) >= 2 {
			counted = append(counted, pair)
		}
	}
	if len(counted) == 0 {
		return nil, nil
	}

	matches := make(map[string]int, len(docIDs))
	for _, pair := range counted {
		matched, err := b.phraseMatchIDs(pair, docIDs)
		if err != nil {
			// All-or-nothing: a partial count would rank hits by which pair
			// happened to finish rather than by phrase share.
			return nil, err
		}
		for id := range matched {
			matches[id]++
		}
	}

	shares := make(map[string]float64, len(matches))
	for id, n := range matches {
		shares[id] = float64(n) / float64(len(counted))
	}
	return shares, nil
}

// applyPhraseBoost applies the mild phrase boost (#2327, see phraseBoost) to a
// relevance-sorted, already-truncated result list: each hit's rank is scaled
// by 1 + phraseBoost × (counted adjacent query word pairs appearing as a
// phrase in its content / pairs counted), then the list is re-sorted. Because
// it runs after the top-N cut, the boost can only reorder the returned hits —
// never change which hits are returned. Fail open: if the phrase matching
// fails, the results keep their unboosted ranks and order, and a warning is
// logged. Caller holds b.mu.
func (b *BleveIndex) applyPhraseBoost(queryStr string, results []Result) {
	docIDs := make([]string, 0, len(results))
	for i := range results {
		docIDs = append(docIDs, results[i].docID)
	}
	shares, err := b.phraseShares(queryStr, docIDs)
	if err != nil {
		memoryLog.Warnf("bleve phrase boost skipped for query %q: %v", queryStr, err)
		return
	}
	for i := range results {
		// ×1 exactly unless this hit carries the query's words next to each
		// other in its content (a nil map gives share 0).
		results[i].Rank *= 1 + phraseBoost*shares[results[i].docID]
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Rank > results[j].Rank })
}

// Search queries the bleve index. sort controls result ordering:
// "relevance" (default/empty) orders by weighted score,
// "newest" orders by mtime descending, "oldest" orders by mtime ascending.
// Any other sort is rejected: the ordering switch would treat it as relevance
// while the re-rank gates skip it, returning an unranked, untruncated 2x
// window. opts provides optional date range filtering.
// On relevance sort, a query of two or more words also gets a mild phrase
// boost (#2327): hits whose content carries the query's words next to each
// other, in order, rank slightly higher (see phraseBoost). The boost is
// applied after the result list is cut to the return limit, so it never
// changes which hits are returned — only their order.
func (b *BleveIndex) Search(queryStr string, sortOrder string, opts *SearchOptions) ([]Result, error) {
	switch sortOrder {
	case "", "relevance", "newest", "oldest":
	default:
		return nil, fmt.Errorf("invalid sort %q (use relevance, newest or oldest)", sortOrder)
	}

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
	req.Size = b.searchLimitLocked()
	req.Fields = []string{"path", "source", "mtime"}

	// Highlight content field for snippets
	req.Highlight = bleve.NewHighlightWithStyle(blevehtml.Name)
	req.Highlight.AddField(bleveContentField)

	switch sortOrder {
	case "newest":
		req.SortBy([]string{"-mtime"})
	case "oldest":
		req.SortBy([]string{"mtime"})
	default:
		// relevance: over-fetch so the recency re-rank (#352) can promote a recent
		// item into the returned set; truncated back to the return limit after.
		req.Size = b.searchLimitLocked() * 2
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
			docID:   hit.ID,
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

	// For relevance sort, re-rank by weighted rank with a recency boost (#352) and
	// truncate to the return limit. When temporal decay is off, hl/boost are 0 so
	// rerankByRecency degenerates to a plain rank-descending sort + truncate —
	// identical top-N to the pre-#352 behaviour.
	if isRelevanceSort(sortOrder) {
		hl, boost := 0.0, 0.0
		if b.temporalDecay {
			hl, boost = b.decayHalfLifeDays, b.decayBoost
		}
		results = rerankByRecency(results, time.Now(), hl, boost, b.evergreenPatterns, b.searchLimitLocked(), true /* bleve rank: higher is better */)
		// Mild phrase boost (#2327), after the cut: the returned set is
		// exactly the unboosted one; only the order within it can change.
		b.applyPhraseBoost(queryStr, results)
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
			memoryLog.Warnf("bleve file watcher error: %v", err)
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
			memoryLog.Errorf("bleve auto-reindex failed: %v", err)
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
