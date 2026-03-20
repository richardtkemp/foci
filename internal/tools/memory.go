package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"foci/internal/memory"
)

// NewMemorySearchTool creates the memory_search tool backed by one or more search backends.
// backends maps backend names (e.g. "fts5", "bleve") to their Searcher implementation.
// order is the config-specified backend order — the first entry is the default.
// If only one backend is configured, the "backend" parameter is hidden from the tool schema.
// convReader provides conversation context lookup (may be nil).
func NewMemorySearchTool(backends map[string]memory.Searcher, order []string, convReader *memory.ConversationReader) *Tool {
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
		Description: "Search memory files and conversation history using full-text search. Supports natural language queries with stemming (e.g., 'programming' matches 'program', 'programmer'). Memory files are ranked higher than conversation history. Sort by relevance (default), newest, or oldest. To retrieve conversation context around a specific result, use the session#rowID shown in results as the query (e.g., 'agent/c123/456#42').",
		Parameters:  schema,
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return memorySearch(ctx, params, backends, names[0], convReader)
		},
	}
}

// dateSchemaProperties is the shared JSON fragment for date_from/date_to schema properties.
const dateSchemaProperties = `"date_from": {
				"type": "string",
				"description": "Filter results to entries on or after this date (YYYY-MM-DD format, e.g., '2024-01-15')"
			},
			"date_to": {
				"type": "string",
				"description": "Filter results to entries on or before this date (YYYY-MM-DD format, e.g., '2024-12-31')"
			}`

// linesSchemaProperty is the shared JSON fragment for the lines parameter.
const linesSchemaProperty = `"lines": {
				"type": "integer",
				"description": "Number of surrounding conversation messages to include for context. When using direct lookup (query='session#rowID'), defaults to 10."
			}`

// buildMemorySearchSchema builds the tool parameter schema.
// When multiple backends are available, includes the "backend" parameter.
func buildMemorySearchSchema(names []string) json.RawMessage {
	if len(names) <= 1 {
		return json.RawMessage(fmt.Sprintf(`{
			"type": "object",
			"properties": {
				"query": {
					"type": "string",
					"description": "Search query (supports natural language with stemming). Use 'session#rowID' from a previous result to retrieve conversation context."
				},
				"sort": {
					"type": "string",
					"enum": ["relevance", "newest", "oldest"],
					"description": "Sort order: relevance (default, weighted by source), newest (most recently modified first), or oldest (least recently modified first)"
				},
				%s,
				%s
			},
			"required": ["query"]
		}`, dateSchemaProperties, linesSchemaProperty))
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
				"description": "Search query (supports natural language with stemming). Use 'session#rowID' from a previous result to retrieve conversation context."
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
			},
			%s,
			%s
		},
		"required": ["query"]
	}`, enumJSON, names[0], dateSchemaProperties, linesSchemaProperty))
}

// parseConversationRef detects the "session#rowID" pattern for direct conversation lookup.
// Returns the session key, row ID, and true if the pattern matches.
func parseConversationRef(query string) (session string, rowID int64, ok bool) {
	idx := strings.LastIndex(query, "#")
	if idx < 1 {
		return "", 0, false
	}
	session = query[:idx]
	id, err := strconv.ParseInt(query[idx+1:], 10, 64)
	if err != nil || id <= 0 {
		return "", 0, false
	}
	// Session keys have at least one slash
	if !strings.Contains(session, "/") {
		return "", 0, false
	}
	return session, id, true
}

func memorySearch(ctx context.Context, params json.RawMessage, backends map[string]memory.Searcher, defaultBackend string, convReader *memory.ConversationReader) (ToolResult, error) {
	var p struct {
		Query    string `json:"query"`
		Sort     string `json:"sort"`
		Backend  string `json:"backend"`
		DateFrom string `json:"date_from"`
		DateTo   string `json:"date_to"`
		Lines    int    `json:"lines"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ToolResult{}, fmt.Errorf("parse params: %w", err)
	}

	// Direct conversation lookup: "session#rowID"
	if session, rowID, ok := parseConversationRef(p.Query); ok {
		return conversationLookup(convReader, session, rowID, p.Lines)
	}

	backendName := p.Backend
	if backendName == "" {
		backendName = defaultBackend
	}

	searcher, ok := backends[backendName]
	if !ok {
		return ToolResult{}, fmt.Errorf("unknown search backend %q", backendName)
	}

	var opts *memory.SearchOptions

	// Exclude the current session's conversation entries from results —
	// finding your own earlier messages is circular and wastes result slots.
	sessionKey := SessionKeyFromContext(ctx)
	if sessionKey != "" {
		if opts == nil {
			opts = &memory.SearchOptions{}
		}
		opts.ExcludePath = sessionKey
	}

	if p.DateFrom != "" || p.DateTo != "" {
		if opts == nil {
			opts = &memory.SearchOptions{}
		}
		if p.DateFrom != "" {
			t, err := time.Parse("2006-01-02", p.DateFrom)
			if err != nil {
				return ToolResult{}, fmt.Errorf("invalid date_from format (use YYYY-MM-DD): %w", err)
			}
			opts.DateFrom = &t
		}
		if p.DateTo != "" {
			t, err := time.Parse("2006-01-02", p.DateTo)
			if err != nil {
				return ToolResult{}, fmt.Errorf("invalid date_to format (use YYYY-MM-DD): %w", err)
			}
			// Exclusive upper bound: start of the next day
			nextDay := t.AddDate(0, 0, 1)
			opts.DateTo = &nextDay
		}
	}

	results, err := searcher.Search(p.Query, p.Sort, opts)
	if err != nil {
		return ToolResult{}, fmt.Errorf("search: %w", err)
	}

	if len(results) == 0 {
		return TextResult("No matches found."), nil
	}

	var sb strings.Builder
	hasConvContext := false
	for _, r := range results {
		formatSearchResult(&sb, r)
		if r.Source == "conversation" && r.RowID > 0 {
			hasConvContext = true
		}

		// If lines requested and conversation result with RowID, show context
		if p.Lines > 0 && r.Source == "conversation" && r.RowID > 0 && convReader != nil {
			msgs, err := convReader.ReadContext(r.Path, r.RowID, p.Lines)
			if err == nil && len(msgs) > 0 {
				for _, m := range msgs {
					marker := "    "
					if m.RowID == r.RowID {
						marker = "  » "
					}
					fmt.Fprintf(&sb, "%s#%d [%s]: %s\n", marker, m.RowID, m.Time.Format("15:04"), truncate(m.Text, 200))
				}
			}
		}
	}
	if hasConvContext && p.Lines == 0 {
		sb.WriteString("\nTip: To see surrounding conversation, re-query with the session#rowID shown above (e.g., query=\"agent/c123/456#42\"), or add \"lines\" to expand inline.\n")
	}
	return TextResult(sb.String()), nil
}

// conversationLookup handles direct "session#rowID" queries by fetching
// surrounding conversation messages.
func conversationLookup(convReader *memory.ConversationReader, session string, rowID int64, lines int) (ToolResult, error) {
	if convReader == nil {
		return ToolResult{}, fmt.Errorf("conversation context not available")
	}
	if lines == 0 {
		lines = 10
	}
	msgs, err := convReader.ReadContext(session, rowID, lines)
	if err != nil {
		return ToolResult{}, fmt.Errorf("read context: %w", err)
	}
	if len(msgs) == 0 {
		return TextResult("No messages found."), nil
	}
	var sb strings.Builder
	for _, m := range msgs {
		marker := "  "
		if m.RowID == rowID {
			marker = "» "
		}
		fmt.Fprintf(&sb, "%s%s#%d [%s]: %s\n", marker, session, m.RowID, m.Time.Format("2006-01-02 15:04"), m.Text)
	}
	return TextResult(sb.String()), nil
}

// formatResult writes a single search result line.
func formatSearchResult(sb *strings.Builder, r memory.Result) {
	ts := ""
	if !r.Time.IsZero() {
		ts = " " + r.Time.Format("2006-01-02 15:04")
	}
	path := r.Path
	if r.Source == "conversation" && r.RowID > 0 {
		path = fmt.Sprintf("%s#%d", r.Path, r.RowID)
	}
	fmt.Fprintf(sb, "[%s%s] %s: %s\n", r.Source, ts, path, r.Snippet)
}

// truncate shortens s to at most max bytes, appending "…" if truncated.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
