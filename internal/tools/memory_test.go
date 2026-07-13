package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/memory"
	"foci/internal/sqlite"
)

func testMemoryTool(t *testing.T) (*Tool, string) {
	t.Helper()
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)
	dbPath := filepath.Join(dir, "memory.db")

	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, err := memory.NewIndex(dbPath, sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { idx.Close() })

	backends := map[string]memory.Searcher{"fts5": idx}
	return NewMemorySearchTool(backends, func() string { return "fts5" }, nil), memDir
}

func TestMemorySearch(t *testing.T) {
	// Verifies that a basic FTS search returns results from multiple indexed files that match the query.
	t.Parallel()
	_, memDir := testMemoryTool(t)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("Remember to buy milk\nThe sky is blue\n"), 0644)
	os.WriteFile(filepath.Join(memDir, "todo.md"), []byte("Buy groceries\nClean house\nBuy a new book\n"), 0644)

	// Re-index after writing files (the index was created before files existed)
	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, _ := memory.NewIndex(filepath.Join(filepath.Dir(memDir), "memory.db"), sources, 0, 0.1)
	defer idx.Close()
	idx.Reindex()

	backends := map[string]memory.Searcher{"fts5": idx}
	tool2 := NewMemorySearchTool(backends, func() string { return "fts5" }, nil)
	params, _ := json.Marshal(map[string]string{"query": "buy"})

	result, err := tool2.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should find "buy" in both files
	if !strings.Contains(result.Text, "notes.md") {
		t.Errorf("missing notes.md in result: %q", result.Text)
	}
	if !strings.Contains(result.Text, "todo.md") {
		t.Errorf("missing todo.md in result: %q", result.Text)
	}

}

func TestMemorySearchNoMatches(t *testing.T) {
	// Verifies that a query with no matching content returns the canonical "No matches found." message.
	t.Parallel()
	_, memDir := testMemoryTool(t)
	os.WriteFile(filepath.Join(memDir, "test.md"), []byte("nothing relevant here\n"), 0644)

	// Need to reindex with a fresh connection to pick up the file
	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, _ := memory.NewIndex(filepath.Join(filepath.Dir(memDir), "memory.db"), sources, 0, 0.1)
	defer idx.Close()
	idx.Reindex()

	backends := map[string]memory.Searcher{"fts5": idx}
	tool := NewMemorySearchTool(backends, func() string { return "fts5" }, nil)
	params, _ := json.Marshal(map[string]string{"query": "xyzzy"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Text != "No matches found." {
		t.Errorf("result = %q", result.Text)
	}
}

func TestMemorySearchEmpty(t *testing.T) {
	// Verifies that searching an empty index (no indexed files) returns the canonical no-results message.
	t.Parallel()
	tool, _ := testMemoryTool(t)
	params, _ := json.Marshal(map[string]string{"query": "anything"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Text != "No matches found." {
		t.Errorf("result = %q", result.Text)
	}
}

func TestMemorySearchShowsSource(t *testing.T) {
	// Verifies that results include source type labels so the caller can distinguish memory files from conversation history.
	t.Parallel()
	_, memDir := testMemoryTool(t)
	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("The weather is sunny today"), 0644)

	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, _ := memory.NewIndex(filepath.Join(filepath.Dir(memDir), "memory.db"), sources, 0, 0.1)
	defer idx.Close()
	idx.Reindex()
	idx.IndexConversation("We talked about the weather yesterday", "main/i0", 1)

	backends := map[string]memory.Searcher{"fts5": idx}
	tool := NewMemorySearchTool(backends, func() string { return "fts5" }, nil)
	params, _ := json.Marshal(map[string]string{"query": "weather"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should show both memory and conversation results with source labels (timestamps may be included)
	if !strings.Contains(result.Text, "[memory") {
		t.Errorf("missing [memory source label in result: %q", result.Text)
	}
	if !strings.Contains(result.Text, "[conversation") {
		t.Errorf("missing [conversation source label in result: %q", result.Text)
	}
}

func TestMemorySearchSortParam(t *testing.T) {
	// Verifies that the sort parameter accepts "newest", "oldest", "relevance", and empty (default) without error, and all return the expected file.
	t.Parallel()
	_, memDir := testMemoryTool(t)

	os.WriteFile(filepath.Join(memDir, "recent.md"), []byte("Recently added content about sorting"), 0644)

	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, _ := memory.NewIndex(filepath.Join(filepath.Dir(memDir), "memory.db"), sources, 0, 0.1)
	defer idx.Close()
	idx.Reindex()

	backends := map[string]memory.Searcher{"fts5": idx}
	tool := NewMemorySearchTool(backends, func() string { return "fts5" }, nil)

	// Test with sort=newest
	params, _ := json.Marshal(map[string]string{"query": "sorting", "sort": "newest"})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute with sort=newest: %v", err)
	}
	if !strings.Contains(result.Text, "recent.md") {
		t.Errorf("missing recent.md in result: %q", result.Text)
	}

	// Test with sort=oldest
	params, _ = json.Marshal(map[string]string{"query": "sorting", "sort": "oldest"})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute with sort=oldest: %v", err)
	}
	if !strings.Contains(result.Text, "recent.md") {
		t.Errorf("missing recent.md in result: %q", result.Text)
	}

	// Test with sort=relevance (explicit)
	params, _ = json.Marshal(map[string]string{"query": "sorting", "sort": "relevance"})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute with sort=relevance: %v", err)
	}
	if !strings.Contains(result.Text, "recent.md") {
		t.Errorf("missing recent.md in result: %q", result.Text)
	}

	// Test with no sort param (default)
	params, _ = json.Marshal(map[string]string{"query": "sorting"})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute with default sort: %v", err)
	}
	if !strings.Contains(result.Text, "recent.md") {
		t.Errorf("missing recent.md in result: %q", result.Text)
	}
}

func TestMemorySearchBackendParam(t *testing.T) {
	// Verifies that when multiple backends are configured, the tool exposes a "backend" parameter in its schema and correctly routes queries to each backend by name.
	t.Parallel()
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)

	os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("Testing backend selection parameter"), 0644)

	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}

	// Create both FTS5 and bleve backends
	fts5Idx, err := memory.NewIndex(filepath.Join(dir, "memory.db"), sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer fts5Idx.Close()
	fts5Idx.Reindex()

	bleveIdx, err := memory.NewBleveIndex(filepath.Join(dir, "memory.bleve"), sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewBleveIndex: %v", err)
	}
	defer bleveIdx.Close()
	bleveIdx.Reindex()

	backends := map[string]memory.Searcher{
		"fts5":  fts5Idx,
		"bleve": bleveIdx,
	}
	tool := NewMemorySearchTool(backends, func() string { return "fts5" }, nil)

	// Tool schema should include "backend" parameter when multiple backends
	schemaStr := string(tool.Parameters)
	if !strings.Contains(schemaStr, "backend") {
		t.Error("schema should include 'backend' parameter when multiple backends configured")
	}

	// Search with explicit backend=fts5
	params, _ := json.Marshal(map[string]string{"query": "backend selection", "backend": "fts5"})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute with backend=fts5: %v", err)
	}
	if !strings.Contains(result.Text, "notes.md") {
		t.Errorf("fts5 backend should find notes.md: %q", result.Text)
	}

	// Search with explicit backend=bleve
	params, _ = json.Marshal(map[string]string{"query": "backend selection", "backend": "bleve"})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute with backend=bleve: %v", err)
	}
	if !strings.Contains(result.Text, "notes.md") {
		t.Errorf("bleve backend should find notes.md: %q", result.Text)
	}

	// Search without backend (should use default)
	params, _ = json.Marshal(map[string]string{"query": "backend selection"})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute with default backend: %v", err)
	}
	if !strings.Contains(result.Text, "notes.md") {
		t.Errorf("default backend should find notes.md: %q", result.Text)
	}
}

func TestMemorySearchSingleBackendHidesParam(t *testing.T) {
	// Verifies that when only one backend is configured, the "backend" parameter is omitted from the tool schema to avoid unnecessary user-facing options.
	t.Parallel()
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)

	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, err := memory.NewIndex(filepath.Join(dir, "memory.db"), sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer idx.Close()

	// Single backend — schema should NOT include "backend" parameter
	backends := map[string]memory.Searcher{"fts5": idx}
	tool := NewMemorySearchTool(backends, func() string { return "fts5" }, nil)

	schemaStr := string(tool.Parameters)
	if strings.Contains(schemaStr, "backend") {
		t.Error("schema should NOT include 'backend' parameter when only one backend configured")
	}
}

func TestMemorySearchExcludesCurrentSession(t *testing.T) {
	// Verifies that conversation results from the current session are excluded
	// from memory_search results, preventing circular self-referencing.
	t.Parallel()
	_, memDir := testMemoryTool(t)

	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, _ := memory.NewIndex(filepath.Join(filepath.Dir(memDir), "memory.db"), sources, 0, 0.1)
	defer idx.Close()

	// Index conversations from two different sessions
	currentSession := "agent1/imain"
	otherSession := "agent1/iother"
	idx.IndexConversation("The platypus is a fascinating mammal", currentSession, 1)
	idx.IndexConversation("The platypus lays eggs unlike most mammals", otherSession, 2)

	backends := map[string]memory.Searcher{"fts5": idx}
	tool := NewMemorySearchTool(backends, func() string { return "fts5" }, nil)
	params, _ := json.Marshal(map[string]string{"query": "platypus"})

	// Search WITH session context — should exclude current session
	ctx := WithSessionKey(context.Background(), currentSession)
	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, otherSession) {
		t.Errorf("should include other session in results: %q", result.Text)
	}
	if strings.Contains(result.Text, currentSession) {
		t.Errorf("should exclude current session from results: %q", result.Text)
	}

	// Search WITHOUT session context — should include both
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, currentSession) || !strings.Contains(result.Text, otherSession) {
		t.Errorf("without session context, should include both sessions: %q", result.Text)
	}
}

func TestMemorySearchDateRange(t *testing.T) {
	// Tests date_from and date_to filtering functionality.
	t.Parallel()
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)

	oldFile := filepath.Join(memDir, "old.md")
	newFile := filepath.Join(memDir, "new.md")

	os.WriteFile(oldFile, []byte("Historical document about project alpha"), 0644)
	oldTime := time.Now().Add(-7 * 24 * time.Hour)
	os.Chtimes(oldFile, oldTime, oldTime)

	os.WriteFile(newFile, []byte("Recent document about project beta"), 0644)

	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, err := memory.NewIndex(filepath.Join(dir, "memory.db"), sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer idx.Close()
	idx.Reindex()

	backends := map[string]memory.Searcher{"fts5": idx}
	tool := NewMemorySearchTool(backends, func() string { return "fts5" }, nil)

	// Search without date filter should find both
	params, _ := json.Marshal(map[string]string{"query": "project"})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute without date filter: %v", err)
	}
	if !strings.Contains(result.Text, "old.md") || !strings.Contains(result.Text, "new.md") {
		t.Errorf("expected both files without date filter: %q", result.Text)
	}

	// Search with date_from should exclude old file
	dateFrom := time.Now().Add(-2 * 24 * time.Hour).Format("2006-01-02")
	params, _ = json.Marshal(map[string]string{"query": "project", "date_from": dateFrom})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute with date_from: %v", err)
	}
	if strings.Contains(result.Text, "old.md") {
		t.Errorf("old.md should be excluded by date_from: %q", result.Text)
	}
	if !strings.Contains(result.Text, "new.md") {
		t.Errorf("new.md should be included by date_from: %q", result.Text)
	}

	// Search with date_to should exclude new file
	dateTo := time.Now().Add(-3 * 24 * time.Hour).Format("2006-01-02")
	params, _ = json.Marshal(map[string]string{"query": "project", "date_to": dateTo})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute with date_to: %v", err)
	}
	if !strings.Contains(result.Text, "old.md") {
		t.Errorf("old.md should be included by date_to: %q", result.Text)
	}
	if strings.Contains(result.Text, "new.md") {
		t.Errorf("new.md should be excluded by date_to: %q", result.Text)
	}
}

func TestMemorySearchDateRangeInvalid(t *testing.T) {
	// Tests that invalid date format returns a clear error.
	t.Parallel()
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)

	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	idx, err := memory.NewIndex(filepath.Join(dir, "memory.db"), sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer idx.Close()

	backends := map[string]memory.Searcher{"fts5": idx}
	tool := NewMemorySearchTool(backends, func() string { return "fts5" }, nil)

	// Invalid date_from format
	params, _ := json.Marshal(map[string]string{"query": "test", "date_from": "2024/01/15"})
	_, err = tool.Execute(context.Background(), params)
	if err == nil {
		t.Error("expected error for invalid date_from format")
	}
	if !strings.Contains(err.Error(), "date_from") {
		t.Errorf("error should mention date_from: %v", err)
	}

	// Invalid date_to format
	params, _ = json.Marshal(map[string]string{"query": "test", "date_to": "Jan 15, 2024"})
	_, err = tool.Execute(context.Background(), params)
	if err == nil {
		t.Error("expected error for invalid date_to format")
	}
	if !strings.Contains(err.Error(), "date_to") {
		t.Errorf("error should mention date_to: %v", err)
	}
}

func TestMemorySearchBleveRowID(t *testing.T) {
	// Verifies that bleve conversation results include the row ID in output (session#rowID format).
	t.Parallel()
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)

	sources := map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}
	bleveIdx, err := memory.NewBleveIndex(filepath.Join(dir, "search.bleve"), sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewBleveIndex: %v", err)
	}
	defer bleveIdx.Close()

	bleveIdx.IndexConversation("The platypus is an unusual creature", "agent/c100", 42)
	// Allow bleve to process
	time.Sleep(50 * time.Millisecond)

	backends := map[string]memory.Searcher{"bleve": bleveIdx}
	tool := NewMemorySearchTool(backends, func() string { return "bleve" }, nil)
	params, _ := json.Marshal(map[string]string{"query": "platypus"})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should include session#rowID in the output
	if !strings.Contains(result.Text, "agent/c100#42") {
		t.Errorf("expected session#rowID in result, got: %q", result.Text)
	}
}

// setupConversationDB creates a conversation DB with test messages and returns the path.
func setupConversationDB(t *testing.T, dir, agentID, session string, messages []string) string {
	t.Helper()
	dbDir := filepath.Join(dir, agentID, ".data")
	os.MkdirAll(dbDir, 0755)
	dbPath := filepath.Join(dbDir, "conversation.db")

	db, err := sqlite.OpenInit(dbPath, `CREATE TABLE IF NOT EXISTS messages (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		ts         TEXT    NOT NULL,
		direction  TEXT    NOT NULL,
		user_id    TEXT    NOT NULL,
		username   TEXT    NOT NULL,
		chat_id    INTEGER NOT NULL,
		text       TEXT    NOT NULL,
		parse_mode TEXT,
		session    TEXT,
		error      TEXT
	)`)
	if err != nil {
		t.Fatalf("create conversation db: %v", err)
	}
	defer func() { _ = db.Close() }()

	baseTime := time.Date(2026, 3, 20, 14, 0, 0, 0, time.UTC)
	for i, msg := range messages {
		ts := baseTime.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano)
		_, err := db.Exec(
			`INSERT INTO messages (ts, direction, user_id, username, chat_id, text, parse_mode, session, error)
			 VALUES (?, 'recv', 'u1', 'user', 100, ?, '', ?, '')`,
			ts, msg, session,
		)
		if err != nil {
			t.Fatalf("insert message: %v", err)
		}
	}
	return dbPath
}

func TestMemorySearchDirectLookup(t *testing.T) {
	// Verifies that using "session#rowID" as query triggers direct conversation lookup
	// and returns surrounding messages with the target marked.
	t.Parallel()
	dir := t.TempDir()
	session := "testagent/c100"
	messages := []string{
		"first message",
		"second message",
		"the important message about permissions",
		"fourth message",
		"fifth message",
	}
	dbPath := setupConversationDB(t, dir, "testagent", session, messages)

	convReader := memory.NewConversationReader(map[string]string{
		"testagent": dbPath,
	})

	// Create a minimal tool — backends aren't used for direct lookup
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)
	sources := map[string]memory.SourceConfig{"memory": {Dir: memDir, Weight: 1.0}}
	idx, _ := memory.NewIndex(filepath.Join(dir, "memory.db"), sources, 0, 0.1)
	defer idx.Close()
	backends := map[string]memory.Searcher{"fts5": idx}
	tool := NewMemorySearchTool(backends, func() string { return "fts5" }, convReader)

	// Row 3 is "the important message about permissions"
	params, _ := json.Marshal(map[string]string{"query": fmt.Sprintf("%s#3", session)})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should contain the target message marked with »
	if !strings.Contains(result.Text, "» "+session+"#3") {
		t.Errorf("expected target message marked with », got: %q", result.Text)
	}
	// Should contain surrounding messages
	if !strings.Contains(result.Text, "permissions") {
		t.Errorf("expected matched message content, got: %q", result.Text)
	}
	// Default lines=10, so should include other messages
	if !strings.Contains(result.Text, "first message") {
		t.Errorf("expected surrounding context messages, got: %q", result.Text)
	}
}

func TestMemorySearchLinesParam(t *testing.T) {
	// Verifies that the lines parameter adds inline conversation context to search results.
	t.Parallel()
	dir := t.TempDir()
	session := "testagent/c100"
	messages := []string{
		"the weather was nice",
		"we discussed the weather forecast",
		"then talked about weather patterns",
		"weather prediction is hard",
		"last message about weather",
	}
	dbPath := setupConversationDB(t, dir, "testagent", session, messages)

	convReader := memory.NewConversationReader(map[string]string{
		"testagent": dbPath,
	})

	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)
	sources := map[string]memory.SourceConfig{"memory": {Dir: memDir, Weight: 1.0}}

	// Use bleve so we get RowIDs
	bleveIdx, err := memory.NewBleveIndex(filepath.Join(dir, "search.bleve"), sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewBleveIndex: %v", err)
	}
	defer bleveIdx.Close()

	// Index conversations with matching row IDs
	for i, msg := range messages {
		bleveIdx.IndexConversation(msg, session, int64(i+1))
	}
	time.Sleep(50 * time.Millisecond)

	backends := map[string]memory.Searcher{"bleve": bleveIdx}
	tool := NewMemorySearchTool(backends, func() string { return "bleve" }, convReader)

	// Search with lines=4 to get context
	params, _ := json.Marshal(map[string]interface{}{
		"query": "forecast",
		"lines": 4,
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should have the search result line
	if !strings.Contains(result.Text, "[conversation") {
		t.Errorf("expected conversation result, got: %q", result.Text)
	}
	// Should have context lines with » marking the matched message
	if !strings.Contains(result.Text, "»") {
		t.Errorf("expected » marker in context, got: %q", result.Text)
	}
}

func TestMemorySearchDirectLookupNoReader(t *testing.T) {
	// Verifies that direct lookup without a ConversationReader returns a clear error.
	t.Parallel()
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	os.MkdirAll(memDir, 0755)
	sources := map[string]memory.SourceConfig{"memory": {Dir: memDir, Weight: 1.0}}
	idx, _ := memory.NewIndex(filepath.Join(dir, "memory.db"), sources, 0, 0.1)
	defer idx.Close()

	backends := map[string]memory.Searcher{"fts5": idx}
	tool := NewMemorySearchTool(backends, func() string { return "fts5" }, nil)

	params, _ := json.Marshal(map[string]string{"query": "agent/c100#42"})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Error("expected error for direct lookup without ConversationReader")
	}
	if !strings.Contains(err.Error(), "conversation context not available") {
		t.Errorf("unexpected error: %v", err)
	}
}
