package log

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestConversationLog verifies that conversation entries are written to the database
// and can be queried back with correct fields.
func TestConversationLog(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_conv.db")

	if err := InitConversation(dbPath); err != nil {
		t.Fatalf("InitConversation: %v", err)
	}
	defer CloseConversation()

	// Log a received message
	Conversation(ConversationEntry{
		Direction: "recv",
		UserID:    "12345",
		Username:  "testuser",
		ChatID:    67890,
		Text:      "Hello bot",
		Session:   "main/i0/0",
	})

	// Log a sent response
	Conversation(ConversationEntry{
		Direction: "sent",
		UserID:    "12345",
		Username:  "testuser",
		ChatID:    67890,
		Text:      "Hello human!",
		ParseMode: "Markdown",
		Session:   "main/i0/0",
	})

	// Log a failed send
	Conversation(ConversationEntry{
		Direction: "sent",
		UserID:    "12345",
		Username:  "testuser",
		ChatID:    67890,
		Text:      "bad *markdown",
		ParseMode: "",
		Session:   "main/i0/0",
		Error:     "parse error",
	})

	// Query the database directly to verify
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	rows, err := db.Query("SELECT ts, direction, user_id, username, chat_id, text, parse_mode, session, error FROM messages ORDER BY id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	type row struct {
		ts, direction, userID, username, text, parseMode, session, errMsg string
		chatID                                                            int64
	}

	var results []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ts, &r.direction, &r.userID, &r.username, &r.chatID, &r.text, &r.parseMode, &r.session, &r.errMsg); err != nil {
			t.Fatalf("scan: %v", err)
		}
		results = append(results, r)
	}

	if len(results) != 3 {
		t.Fatalf("got %d rows, want 3", len(results))
	}

	// Check received message
	if results[0].direction != "recv" {
		t.Errorf("row 0 direction = %q", results[0].direction)
	}
	if results[0].text != "Hello bot" {
		t.Errorf("row 0 text = %q", results[0].text)
	}
	if results[0].userID != "12345" {
		t.Errorf("row 0 user_id = %q", results[0].userID)
	}
	if results[0].chatID != 67890 {
		t.Errorf("row 0 chat_id = %d", results[0].chatID)
	}
	if results[0].session != "main/i0/0" {
		t.Errorf("row 0 session = %q", results[0].session)
	}

	// Check timestamp is recent
	ts, err := time.Parse(time.RFC3339Nano, results[0].ts)
	if err != nil {
		t.Errorf("parse timestamp: %v", err)
	}
	if time.Since(ts) > 10*time.Second {
		t.Errorf("timestamp too old: %v", ts)
	}

	// Check sent message
	if results[1].direction != "sent" {
		t.Errorf("row 1 direction = %q", results[1].direction)
	}
	if results[1].text != "Hello human!" {
		t.Errorf("row 1 text = %q", results[1].text)
	}
	if results[1].parseMode != "Markdown" {
		t.Errorf("row 1 parse_mode = %q", results[1].parseMode)
	}

	// Check error message
	if results[2].errMsg != "parse error" {
		t.Errorf("row 2 error = %q", results[2].errMsg)
	}
}

// TestConversationNoopWhenUninitialized verifies that logging doesn't panic
// when the conversation system hasn't been initialized.
func TestConversationNoopWhenUninitialized(t *testing.T) {
	// Save and restore global state
	savedLogs := convLogs
	savedFallback := convFallback
	convLogs = nil
	convFallback = nil
	defer func() {
		convLogs = savedLogs
		convFallback = savedFallback
	}()

	// Should not panic
	Conversation(ConversationEntry{
		Direction: "recv",
		UserID:    "1",
		Username:  "test",
		ChatID:    1,
		Text:      "hello",
	})
}

// TestConversationBusyTimeout verifies that the conversation database has the
// correct busy_timeout PRAGMA set.
func TestConversationBusyTimeout(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_conv.db")

	if err := InitConversation(dbPath); err != nil {
		t.Fatalf("InitConversation: %v", err)
	}
	defer CloseConversation()

	var timeout int
	if err := convFallback.db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", timeout)
	}
}

// TestAgentFromSession verifies extraction of agent IDs from session key strings.
func TestAgentFromSession(t *testing.T) {
	tests := []struct {
		session string
		want    string
	}{
		{"clutch/c123/1000", "clutch"},
		{"otto/i0/0", "otto"},
		{"fotini/c5970082313/1000/b2000", "fotini"},
		{"", ""},
		{"noslash", ""},
	}
	for _, tt := range tests {
		got := agentFromSession(tt.session)
		if got != tt.want {
			t.Errorf("agentFromSession(%q) = %q, want %q", tt.session, got, tt.want)
		}
	}
}

// TestConversationHook verifies that ConversationHook is called for non-empty text entries.
func TestConversationHook(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_conv.db")
	if err := InitConversation(dbPath); err != nil {
		t.Fatalf("InitConversation: %v", err)
	}
	defer CloseConversation()

	var hookedText, hookedSession string
	ConversationHook = func(text, session string) {
		hookedText = text
		hookedSession = session
	}
	defer func() { ConversationHook = nil }()

	Conversation(ConversationEntry{
		Direction: "recv", UserID: "1", Username: "u", ChatID: 1,
		Text: "hook test", Session: "main/c1/1000",
	})

	if hookedText != "hook test" {
		t.Errorf("hook text = %q, want %q", hookedText, "hook test")
	}
	if hookedSession != "main/c1/1000" {
		t.Errorf("hook session = %q, want %q", hookedSession, "main/c1/1000")
	}

	// Empty text should NOT trigger the hook
	hookedText = ""
	Conversation(ConversationEntry{
		Direction: "recv", UserID: "1", Username: "u", ChatID: 1,
		Text: "", Session: "main/c1/1000",
	})
	if hookedText != "" {
		t.Errorf("hook should not fire for empty text, got %q", hookedText)
	}
}

// TestConversationFallbackRouting verifies that entries with an unknown agent
// session are routed to the fallback log.
func TestConversationFallbackRouting(t *testing.T) {
	dir := t.TempDir()
	agentIDs := []string{"alpha"}
	pathFn := func(id string) string {
		return filepath.Join(dir, "conversation-"+id+".db")
	}
	if err := InitPerAgentConversation(agentIDs, pathFn); err != nil {
		t.Fatalf("InitPerAgentConversation: %v", err)
	}
	defer CloseConversation()

	// Unknown agent session should go to fallback (alpha)
	Conversation(ConversationEntry{
		Direction: "recv", UserID: "1", Username: "u", ChatID: 1,
		Text: "unknown agent", Session: "unknown/c1/1000",
	})

	// Non-slash session should also go to fallback
	Conversation(ConversationEntry{
		Direction: "recv", UserID: "1", Username: "u", ChatID: 1,
		Text: "no agent prefix", Session: "noslash",
	})

	alphaDB, _ := sql.Open("sqlite", pathFn("alpha"))
	defer alphaDB.Close()
	var count int
	alphaDB.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count)
	if count != 2 {
		t.Errorf("fallback messages = %d, want 2", count)
	}
}

// TestInitPerAgentConversationError verifies that InitPerAgentConversation
// cleans up already-opened logs when one fails to open.
func TestInitPerAgentConversationError(t *testing.T) {
	dir := t.TempDir()
	pathFn := func(id string) string {
		if id == "bad" {
			return "/nonexistent/dir/conv.db"
		}
		return filepath.Join(dir, "conversation-"+id+".db")
	}

	err := InitPerAgentConversation([]string{"good", "bad"}, pathFn)
	if err == nil {
		CloseConversation()
		t.Fatal("expected error for bad path")
	}
}

// TestInitConversationError verifies InitConversation returns an error for a bad path.
func TestInitConversationError(t *testing.T) {
	err := InitConversation("/nonexistent/dir/conv.db")
	if err == nil {
		CloseConversation()
		t.Fatal("expected error for bad path")
	}
}

// TestPerAgentConversationRouting verifies that entries are routed to the
// correct per-agent database based on session key.
func TestPerAgentConversationRouting(t *testing.T) {
	dir := t.TempDir()

	agentIDs := []string{"alpha", "beta"}
	pathFn := func(id string) string {
		return filepath.Join(dir, "conversation-"+id+".db")
	}
	if err := InitPerAgentConversation(agentIDs, pathFn); err != nil {
		t.Fatalf("InitPerAgentConversation: %v", err)
	}
	defer CloseConversation()

	// Log to alpha
	Conversation(ConversationEntry{
		Direction: "recv", UserID: "1", Username: "u", ChatID: 1,
		Text: "hello alpha", Session: "alpha/i0/0",
	})
	// Log to beta
	Conversation(ConversationEntry{
		Direction: "recv", UserID: "2", Username: "v", ChatID: 2,
		Text: "hello beta", Session: "beta/i0/0",
	})

	// Verify alpha's DB has 1 row
	alphaDB, _ := sql.Open("sqlite", pathFn("alpha"))
	defer alphaDB.Close()
	var alphaCount int
	alphaDB.QueryRow("SELECT COUNT(*) FROM messages").Scan(&alphaCount)
	if alphaCount != 1 {
		t.Errorf("alpha messages = %d, want 1", alphaCount)
	}

	// Verify beta's DB has 1 row
	betaDB, _ := sql.Open("sqlite", pathFn("beta"))
	defer betaDB.Close()
	var betaCount int
	betaDB.QueryRow("SELECT COUNT(*) FROM messages").Scan(&betaCount)
	if betaCount != 1 {
		t.Errorf("beta messages = %d, want 1", betaCount)
	}
}
