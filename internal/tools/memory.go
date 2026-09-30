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
// defaultBackend returns the preferred backend name, read fresh on each call so a
// live config edit takes effect immediately; the initial value also orders the
// schema's "backend" enum (fixed at construction — only the enum default label
// is illustrative, not authoritative). If only one backend exists, the
// "backend" parameter is hidden from the tool schema.
// convReader provides conversation context lookup (may be nil).
func NewMemorySearchTool(backends map[string]memory.Searcher, defaultBackend func() string, convReader *memory.ConversationReader) *Tool {
	// Build ordered name list: default first, then others.
	initial := defaultBackend()
	names := make([]string, 0, len(backends))
	if _, ok := backends[initial]; ok {
		names = append(names, initial)
	}
	for n := range backends {
		if n != initial {
			names = append(names, n)
		}
	}
	fallback := names[0]

	// Build the JSON schema dynamically
	schema := buildMemorySearchSchema(names)

	return &Tool{
		Name:        "memory_search",
		ExecExport:  true,
		Positional:  []string{"query"},
		JSONOutput:  memorySearchJSONOutput,
		Description: "Search memory files and conversation history using full-text search. Supports natural language queries with stemming (e.g., 'programming' matches 'program', 'programmer'). Memory files are ranked higher than conversation history. Sort by relevance (default), newest, or oldest. To retrieve conversation context around a specific result, use the session#rowID shown in results as the query (e.g., 'agent/c123#42'). Conversation hits written by a subagent, or by a turn that was never delivered to chat (reflection, background, consolidation, /branch), are labelled with that kind, e.g. [conversation/subagent ...] or [conversation/reflection ...].",
		Parameters:  schema,
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			def := defaultBackend()
			if _, ok := backends[def]; !ok {
				def = fallback
			}
			return memorySearch(ctx, params, backends, def, convReader)
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
	p, err := UnmarshalParams[struct {
		Query    string `json:"query"`
		Sort     string `json:"sort"`
		Backend  string `json:"backend"`
		DateFrom string `json:"date_from"`
		DateTo   string `json:"date_to"`
		Lines    int    `json:"lines"`
	}](params)
	if err != nil {
		return ToolResult{}, err
	}

	// Direct conversation lookup: "session#rowID"
	if session, rowID, ok := parseConversationRef(p.Query); ok {
		return conversationLookup(convReader, session, rowID, p.Lines, WantsJSON(ctx))
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

	if len(results) == 0 && !WantsJSON(ctx) {
		return TextResult("No matches found."), nil
	}

	// Label subagent and non-delivered-turn hits (#2060) so they don't read as chat.
	var refs []memory.ConversationRef
	for _, r := range results {
		if r.Source == "conversation" && r.RowID > 0 {
			refs = append(refs, memory.ConversationRef{Session: r.Path, RowID: r.RowID})
		}
	}
	kinds := convReader.Kinds(refs)

	if WantsJSON(ctx) {
		return memorySearchJSON(results, kinds, convReader, p.Lines)
	}

	var sb strings.Builder
	hasConvContext := false
	for _, r := range results {
		formatSearchResult(&sb, r, kinds[memory.ConversationRef{Session: r.Path, RowID: r.RowID}])
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
					fmt.Fprintf(&sb, "%s#%d [%s]%s: %s\n", marker, m.RowID, m.Time.Format("15:04"), kindTag(m.Kind), truncate(m.Text, 200))
				}
			}
		}
	}
	if hasConvContext && p.Lines == 0 {
		sb.WriteString("\nTip: To see surrounding conversation, re-query with the session#rowID shown above (e.g., query=\"agent/c123#42\"), or add \"lines\" to expand inline.\n")
	}
	return TextResult(sb.String()), nil
}

// conversationLookup handles direct "session#rowID" queries by fetching
// surrounding conversation messages.
func conversationLookup(convReader *memory.ConversationReader, session string, rowID int64, lines int, asJSON bool) (ToolResult, error) {
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
	if asJSON {
		return JSONResult(memoryMessagesJSON(msgs, session, rowID))
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
		fmt.Fprintf(&sb, "%s%s#%d [%s]%s: %s\n", marker, session, m.RowID, m.Time.Format("2006-01-02 15:04"), kindTag(m.Kind), m.Text)
	}
	return TextResult(sb.String()), nil
}

// memorySearchHitJSON is one --json memory_search result (#1215).
type memorySearchHitJSON struct {
	Source  string `json:"source"`         // "memory", "code", "docs", "conversation", ...
	Kind    string `json:"kind,omitempty"` // conversation row kind, e.g. "subagent"; omitted for ordinary rows
	Time    string `json:"time,omitempty"` // RFC 3339; message time, or file mtime
	Path    string `json:"path"`           // file path, or the session key for a conversation hit
	RowID   int64  `json:"row_id,omitempty"`
	Ref     string `json:"ref,omitempty"` // "session#rowID": pass as the query to read the surrounding conversation
	Snippet string `json:"snippet"`
	// Context is the surrounding conversation, present when lines was given.
	Context []memoryMessageJSON `json:"context,omitempty"`
}

// memoryMessageJSON is one conversation message in --json output.
type memoryMessageJSON struct {
	Session string `json:"session"`
	RowID   int64  `json:"row_id"`
	Time    string `json:"time"`
	Kind    string `json:"kind,omitempty"`
	Text    string `json:"text"`
	Match   bool   `json:"match,omitempty"` // the row the search hit or the lookup named
}

const memorySearchJSONOutput = `a JSON array of hits: [{"source", "kind"?, "time"?, "path", "row_id"?, "ref"?, "snippet", "context"?}], [] when nothing matched. "ref" (session#rowID) is set on conversation hits; "context" (with lines) is the surrounding messages [{"session", "row_id", "time", "kind"?, "text", "match"?}]. A session#rowID query prints that message array directly. Message text is full-length, not cut as in the text output.`

func memorySearchJSON(results []memory.Result, kinds map[memory.ConversationRef]string, convReader *memory.ConversationReader, lines int) (ToolResult, error) {
	hits := make([]memorySearchHitJSON, 0, len(results))
	for _, r := range results {
		h := memorySearchHitJSON{Source: r.Source, Path: r.Path, Snippet: r.Snippet}
		if !r.Time.IsZero() {
			h.Time = r.Time.Format(time.RFC3339)
		}
		if r.Source == "conversation" && r.RowID > 0 {
			h.RowID = r.RowID
			h.Ref = fmt.Sprintf("%s#%d", r.Path, r.RowID)
			h.Kind = kinds[memory.ConversationRef{Session: r.Path, RowID: r.RowID}]
			if lines > 0 && convReader != nil {
				if msgs, err := convReader.ReadContext(r.Path, r.RowID, lines); err == nil {
					h.Context = memoryMessagesJSON(msgs, r.Path, r.RowID)
				}
			}
		}
		hits = append(hits, h)
	}
	return JSONResult(hits)
}

// memoryMessagesJSON converts conversation messages for --json output, marking
// the row the caller asked about. Never nil, so an empty lookup prints [].
func memoryMessagesJSON(msgs []memory.ConversationMessage, session string, rowID int64) []memoryMessageJSON {
	out := make([]memoryMessageJSON, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, memoryMessageJSON{
			Session: session, RowID: m.RowID, Time: m.Time.Format(time.RFC3339),
			Kind: m.Kind, Text: m.Text, Match: m.RowID == rowID,
		})
	}
	return out
}

// formatSearchResult writes a single search result line. kind is the
// conversation row's kind (memory.ConversationMessage.Kind), shown after the
// source as "[conversation/subagent ...]"; "" for everything else.
func formatSearchResult(sb *strings.Builder, r memory.Result, kind string) {
	ts := ""
	if !r.Time.IsZero() {
		ts = " " + r.Time.Format("2006-01-02 15:04")
	}
	path := r.Path
	if r.Source == "conversation" && r.RowID > 0 {
		path = fmt.Sprintf("%s#%d", r.Path, r.RowID)
	}
	source := r.Source
	if kind != "" {
		source += "/" + kind
	}
	fmt.Fprintf(sb, "[%s%s] %s: %s\n", source, ts, path, r.Snippet)
}

// kindTag renders a context line's row kind as " (kind)", or "" for ordinary
// conversation.
func kindTag(kind string) string {
	if kind == "" {
		return ""
	}
	return " (" + kind + ")"
}

// truncate shortens s to at most max bytes, appending "…" if truncated.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
