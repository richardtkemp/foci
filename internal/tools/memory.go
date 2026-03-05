package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"foci/internal/memory"
)

// NewMemorySearchTool creates the memory_search tool backed by one or more search backends.
// backends maps backend names (e.g. "fts5", "bleve") to their Searcher implementation.
// order is the config-specified backend order — the first entry is the default.
// If only one backend is configured, the "backend" parameter is hidden from the tool schema.
func NewMemorySearchTool(backends map[string]memory.Searcher, order []string) *Tool {
	// Use config order; only include names that are actually in the backends map.
	names := make([]string, 0, len(order))
	for _, n := range order {
		if _, ok := backends[n]; ok {
			names = append(names, n)
		}
	}

	// Build the JSON schema dynamically
	schema := buildMemorySearchSchema(names)

	return &Tool{
		Name:        "memory_search",
		ExecExport:  true,
		Description: "Search memory files and conversation history using full-text search. Supports natural language queries with stemming (e.g., 'programming' matches 'program', 'programmer'). Memory files are ranked higher than conversation history. Sort by relevance (default), newest, or oldest.",
		Parameters:  schema,
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return memorySearch(ctx, params, backends, names[0])
		},
	}
}

// buildMemorySearchSchema builds the tool parameter schema.
// When multiple backends are available, includes the "backend" parameter.
func buildMemorySearchSchema(names []string) json.RawMessage {
	if len(names) <= 1 {
		return json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {
					"type": "string",
					"description": "Search query (supports natural language with stemming)"
				},
				"sort": {
					"type": "string",
					"enum": ["relevance", "newest", "oldest"],
					"description": "Sort order: relevance (default, weighted by source), newest (most recently modified first), or oldest (least recently modified first)"
				}
			},
			"required": ["query"]
		}`)
	}

	// Build backend enum JSON
	enumParts := make([]string, len(names))
	for i, n := range names {
		enumParts[i] = fmt.Sprintf("%q", n)
	}
	enumJSON := "[" + strings.Join(enumParts, ", ") + "]"

	return json.RawMessage(fmt.Sprintf(`{
		"type": "object",
		"properties": {
			"query": {
				"type": "string",
				"description": "Search query (supports natural language with stemming)"
			},
			"sort": {
				"type": "string",
				"enum": ["relevance", "newest", "oldest"],
				"description": "Sort order: relevance (default, weighted by source), newest (most recently modified first), or oldest (least recently modified first)"
			},
			"backend": {
				"type": "string",
				"enum": %s,
				"description": "Search backend to query (default: %s)"
			}
		},
		"required": ["query"]
	}`, enumJSON, names[0]))
}

func memorySearch(ctx context.Context, params json.RawMessage, backends map[string]memory.Searcher, defaultBackend string) (ToolResult, error) {
	var p struct {
		Query   string `json:"query"`
		Sort    string `json:"sort"`
		Backend string `json:"backend"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ToolResult{}, fmt.Errorf("parse params: %w", err)
	}

	backendName := p.Backend
	if backendName == "" {
		backendName = defaultBackend
	}

	searcher, ok := backends[backendName]
	if !ok {
		return ToolResult{}, fmt.Errorf("unknown search backend %q", backendName)
	}

	results, err := searcher.Search(p.Query, p.Sort)
	if err != nil {
		return ToolResult{}, fmt.Errorf("search: %w", err)
	}

	if len(results) == 0 {
		return TextResult("No matches found."), nil
	}

	var sb strings.Builder
	for _, r := range results {
		fmt.Fprintf(&sb, "[%s] %s: %s\n", r.Source, r.Path, r.Snippet)
	}
	return TextResult(sb.String()), nil
}
