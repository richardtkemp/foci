package memory

// Searcher is the interface that memory search backends implement.
// Both FTS5 (Index) and Bleve (BleveIndex) satisfy this interface.
type Searcher interface {
	// Search queries the index with the given query string.
	// sort controls result ordering: "relevance" (default), "newest", or "oldest".
	Search(query, sort string) ([]Result, error)
}
