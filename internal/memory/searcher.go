package memory

import "time"

// SearchOptions contains optional parameters for memory search.
type SearchOptions struct {
	DateFrom *time.Time // Only include results from this date onwards (inclusive)
	DateTo   *time.Time // Only include results up to this date (inclusive)
}

// Searcher is the interface that memory search backends implement.
// Both FTS5 (Index) and Bleve (BleveIndex) satisfy this interface.
type Searcher interface {
	// Search queries the index with the given query string.
	// sort controls result ordering: "relevance" (default), "newest", or "oldest".
	// opts provides optional filtering parameters (may be nil).
	Search(query, sort string, opts *SearchOptions) ([]Result, error)
}
