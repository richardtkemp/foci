package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"clod/memory"
)

func NewMemorySearchTool(idx *memory.Index) *Tool {
	return &Tool{
		Name:        "memory_search",
		Description: "Search memory files and conversation history using full-text search. Supports natural language queries with stemming (e.g., 'programming' matches 'program', 'programmer'). Memory files are ranked higher than conversation history. Sort by relevance (default) or recency (newest files first).",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {
					"type": "string",
					"description": "Search query (supports natural language with stemming)"
				},
				"sort": {
					"type": "string",
					"enum": ["relevance", "recency"],
					"description": "Sort order: relevance (default) or recency (newest files first)"
				}
			},
			"required": ["query"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return memorySearch(ctx, params, idx)
		},
	}
}

func memorySearch(ctx context.Context, params json.RawMessage, idx *memory.Index) (string, error) {
	var p struct {
		Query string `json:"query"`
		Sort  string `json:"sort"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("parse params: %w", err)
	}

	results, err := idx.Search(p.Query, p.Sort)
	if err != nil {
		return "", fmt.Errorf("search: %w", err)
	}

	if len(results) == 0 {
		return "No matches found.", nil
	}

	var sb strings.Builder
	for _, r := range results {
		fmt.Fprintf(&sb, "[%s] %s: %s\n", r.Source, r.Path, r.Snippet)
	}
	return sb.String(), nil
}
