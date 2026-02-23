package log

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

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
		Session:   "agent:main:main",
	})

	// Log a sent response
	Conversation(ConversationEntry{
		Direction: "sent",
		UserID:    "12345",
		Username:  "testuser",
		ChatID:    67890,
		Text:      "Hello human!",
		ParseMode: "Markdown",
		Session:   "agent:main:main",
	})

	// Log a failed send
	Conversation(ConversationEntry{
		Direction: "sent",
		UserID:    "12345",
		Username:  "testuser",
		ChatID:    67890,
		Text:      "bad *markdown",
		ParseMode: "",
		Session:   "agent:main:main",
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
	if results[0].session != "agent:main:main" {
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

func TestConversationNoopWhenUninitialized(t *testing.T) {
	// Save and restore global state
	saved := convLog
	convLog = nil
	defer func() { convLog = saved }()

	// Should not panic
	Conversation(ConversationEntry{
		Direction: "recv",
		UserID:    "1",
		Username:  "test",
		ChatID:    1,
		Text:      "hello",
	})
}

func TestConversationBusyTimeout(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_conv.db")

	if err := InitConversation(dbPath); err != nil {
		t.Fatalf("InitConversation: %v", err)
	}
	defer CloseConversation()

	var timeout int
	if err := convLog.db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", timeout)
	}
}
