package memory

import (
	"fmt"
	"os"
	"time"

	"github.com/fsnotify/fsnotify"
)

// SearchOptions contains optional parameters for memory search.
type SearchOptions struct {
	DateFrom    *time.Time // Only include results from this date onwards (inclusive)
	DateTo      *time.Time // Only include results before this date (exclusive upper bound; typically start of next day)
	ExcludePath string     // Exclude results whose path equals this value (used to filter out the current session's conversation entries)
}

// Searcher is the interface that memory search backends implement.
// Both FTS5 (Index) and Bleve (BleveIndex) satisfy this interface.
type Searcher interface {
	// Search queries the index with the given query string.
	// sort controls result ordering: "relevance" (default), "newest", or "oldest".
	// opts provides optional filtering parameters (may be nil).
	Search(query, sort string, opts *SearchOptions) ([]Result, error)
}

// watchSources adds each existing source directory to the watcher, skipping
// directories that don't exist yet. On any other error the watcher is closed.
func watchSources(watcher *fsnotify.Watcher, sources map[string]SourceConfig) error {
	for _, cfg := range sources {
		if _, err := os.Stat(cfg.Dir); os.IsNotExist(err) {
			continue
		}
		if err := watcher.Add(cfg.Dir); err != nil {
			_ = watcher.Close()
			return fmt.Errorf("watch %s: %w", cfg.Dir, err)
		}
	}
	return nil
}
