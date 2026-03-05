package memory

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"foci/internal/log"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search"
	blevehtml "github.com/blevesearch/bleve/v2/search/highlight/format/html"
	"github.com/blevesearch/bleve/v2/search/query"
	"github.com/fsnotify/fsnotify"
)

// BleveIndex manages a bleve full-text search index over memory files.
// Unlike the FTS5 Index, it does not index conversation history — only files.
type BleveIndex struct {
	indexPath    string
	index        bleve.Index
	sources      map[string]SourceConfig
	watcher      *fsnotify.Watcher
	debounce     time.Duration
	reindexTimer *time.Timer
	sweepStop    chan struct{}
	closed       bool
	mu           sync.Mutex
}

// buildBleveMapping creates the index mapping for memory documents.
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

	docMapping := bleve.NewDocumentMapping()
	docMapping.AddFieldMappingsAt("content", contentField)
	docMapping.AddFieldMappingsAt("path", pathField)
	docMapping.AddFieldMappingsAt("source", sourceField)
	docMapping.AddFieldMappingsAt("mtime", mtimeField)

	indexMapping := bleve.NewIndexMapping()
	indexMapping.DefaultMapping = docMapping
	indexMapping.DefaultAnalyzer = "en" // English analyzer with Porter stemming
	return indexMapping
}

// NewBleveIndex creates or opens a bleve index at indexPath, indexing .md files
// from the given sources. debounce is the delay before auto-reindexing on file change.
func NewBleveIndex(indexPath string, sources map[string]SourceConfig, debounce time.Duration) (*BleveIndex, error) {
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
		indexPath: indexPath,
		index:     idx,
		sources:   sources,
		debounce:  debounce,
	}, nil
}

// Reindex scans all configured source directories and rebuilds the index.
// The index is closed, removed, and recreated to ensure a clean state.
func (b *BleveIndex) Reindex() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Close existing index, remove, recreate
	if err := b.index.Close(); err != nil {
		return fmt.Errorf("close bleve index for reindex: %w", err)
	}
	if err := os.RemoveAll(b.indexPath); err != nil {
		return fmt.Errorf("remove bleve index: %w", err)
	}

	idx, err := bleve.New(b.indexPath, buildBleveMapping())
	if err != nil {
		return fmt.Errorf("recreate bleve index: %w", err)
	}
	b.index = idx

	// Index all source directories using a batch
	batch := b.index.NewBatch()
	for sourceName, sourceCfg := range b.sources {
		if err := filepath.Walk(sourceCfg.Dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			if !strings.HasSuffix(path, ".md") {
				return nil
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return nil // skip unreadable
			}
			if len(data) == 0 {
				return nil
			}

			relPath, _ := filepath.Rel(sourceCfg.Dir, path)
			docID := sourceName + ":" + relPath
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
	}

	return b.index.Batch(batch)
}

// Search queries the bleve index. sort controls result ordering:
// "relevance" (default/empty) orders by weighted score,
// "newest" orders by mtime descending, "oldest" orders by mtime ascending.
// opts provides optional date range filtering.
func (b *BleveIndex) Search(queryStr string, sortOrder string, opts *SearchOptions) ([]Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Build the main query
	mainQuery := bleve.NewQueryStringQuery(queryStr)

	var finalQuery query.Query = mainQuery

	// Apply date range filter if provided
	if opts != nil && (opts.DateFrom != nil || opts.DateTo != nil) {
		minInclusive := boolPtr(true)
		maxInclusive := boolPtr(opts.DateTo == nil) // exclusive upper bound when DateTo is set
		dateQuery := query.NewNumericRangeInclusiveQuery(
			floatPtrFromTime(opts.DateFrom),
			floatPtrFromTime(opts.DateTo),
			minInclusive,
			maxInclusive,
		)
		dateQuery.SetField("mtime")

		// Combine with AND conjunction
		conjunction := bleve.NewConjunctionQuery(mainQuery, dateQuery)
		finalQuery = conjunction
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
		if cfg, ok := b.sources[source]; ok {
			rank *= (1.0 + cfg.Weight)
		}

		results = append(results, Result{
			Path:    path,
			Snippet: snippet,
			Source:  source,
			Rank:    rank,
		})
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

	for _, cfg := range b.sources {
		if err := watcher.Add(cfg.Dir); err != nil {
			_ = watcher.Close()
			return fmt.Errorf("watch %s: %w", cfg.Dir, err)
		}
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
