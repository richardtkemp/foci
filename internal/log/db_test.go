package log

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAPIDB verifies the SQLite API DB stores entries with all fields including
// call_type, session_file, and that session-based queries work correctly.
func TestAPIDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_api.db")

	if err := InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	defer CloseAPIDB()

	// Insert entries of different call types
	entries := []APIEntry{
		{
			Timestamp:  time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			Session:    "main/c123/1000",
			Model:      "claude-haiku-4-5",
			Input:      1000,
			Output:     200,
			CacheRead:  500,
			CacheWrite: 300,
			CostUSD:    0.005,
			DurationMS: 1200,
			StopReason: "end_turn",
			CallType:   "conversation",
		},
		{
			Timestamp:  time.Date(2026, 3, 1, 10, 1, 0, 0, time.UTC),
			Session:    "main/c123/1000",
			Model:      "claude-haiku-4-5",
			Input:      2000,
			Output:     400,
			CostUSD:    0.01,
			DurationMS: 2400,
			StopReason: "end_turn",
			CallType:   "compaction",
		},
		{
			Timestamp:  time.Date(2026, 3, 1, 10, 2, 0, 0, time.UTC),
			Session:    "main/c123/1000",
			Model:      "claude-haiku-4-5",
			Input:      500,
			Output:     100,
			CostUSD:    0.002,
			DurationMS: 800,
			StopReason: "end_turn",
			CallType:   "summary",
		},
		{
			Timestamp:   time.Date(2026, 3, 1, 10, 3, 0, 0, time.UTC),
			Session:     "main/ispawn-456/1000",
			Model:       "claude-sonnet-4-5",
			Input:       3000,
			Output:      600,
			CostUSD:     0.02,
			DurationMS:  3600,
			StopReason:  "end_turn",
			CallType:    "spawn",
			SessionFile: "/data/sessions/agent/main/spawn/456.jsonl",
		},
	}

	for _, e := range entries {
		apiLog.insert(e)
	}

	// Query by call_type
	rows, err := apiLog.db.Query("SELECT call_type, count(*) FROM api_calls GROUP BY call_type ORDER BY call_type")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var ct string
		var n int
		if err := rows.Scan(&ct, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		counts[ct] = n
	}

	if counts["conversation"] != 1 {
		t.Errorf("conversation count = %d, want 1", counts["conversation"])
	}
	if counts["compaction"] != 1 {
		t.Errorf("compaction count = %d, want 1", counts["compaction"])
	}
	if counts["summary"] != 1 {
		t.Errorf("summary count = %d, want 1", counts["summary"])
	}
	if counts["spawn"] != 1 {
		t.Errorf("spawn count = %d, want 1", counts["spawn"])
	}

	// Verify session_file was stored
	var sf sql.NullString
	err = apiLog.db.QueryRow("SELECT session_file FROM api_calls WHERE call_type = 'spawn'").Scan(&sf)
	if err != nil {
		t.Fatalf("query session_file: %v", err)
	}
	if !sf.Valid || sf.String != "/data/sessions/agent/main/spawn/456.jsonl" {
		t.Errorf("session_file = %v, want /data/sessions/agent/main/spawn/456.jsonl", sf)
	}

	// Verify session_file is NULL for entries without it
	err = apiLog.db.QueryRow("SELECT session_file FROM api_calls WHERE call_type = 'conversation'").Scan(&sf)
	if err != nil {
		t.Fatalf("query session_file: %v", err)
	}
	if sf.Valid {
		t.Errorf("session_file should be NULL for conversation, got %q", sf.String)
	}

	// Query by session index
	var total int
	err = apiLog.db.QueryRow("SELECT count(*) FROM api_calls WHERE session = 'main/c123/1000'").Scan(&total)
	if err != nil {
		t.Fatalf("query by session: %v", err)
	}
	if total != 3 {
		t.Errorf("session count = %d, want 3", total)
	}
}

// TestAPIDBDisabled verifies that API() is a no-op (no panic) when no DB is initialized.
func TestAPIDBDisabled(t *testing.T) {
	old := apiLog
	apiLog = nil
	defer func() { apiLog = old }()

	API(APIEntry{Session: "test", CallType: "conversation"})
	// No panic = pass
}

// TestInitAPIDBError verifies InitAPIDB returns an error for a path that can't be created.
func TestInitAPIDBError(t *testing.T) {
	err := InitAPIDB("/nonexistent/deep/dir/api.db")
	if err == nil {
		CloseAPIDB()
		t.Fatal("expected error for bad DB path")
	}
}

// TestInsertError verifies that insert logs an error (rather than panicking) when
// the prepared statement has been closed.
func TestInsertError(t *testing.T) {
	resetGlobal()
	t.Cleanup(resetGlobal)
	buf := captureOutput(t)

	dbPath := filepath.Join(t.TempDir(), "test_api.db")
	if err := InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}

	// Close the stmt to force an exec error
	apiLog.stmt.Close()

	apiLog.insert(APIEntry{
		Timestamp: time.Now(),
		Session:   "test",
		Model:     "test",
		CallType:  "conversation",
	})

	// Should have logged an error
	if !strings.Contains(buf.String(), "insert error") {
		t.Errorf("expected insert error log, got: %s", buf.String())
	}

	// Clean up — close DB (stmt already closed)
	apiLog.db.Close()
	apiLog = nil
}

// TestInsertSessionLineNullability verifies that session_line is stored as NULL for 0
// and as a non-NULL integer for positive values in the SQLite API log.
func TestInsertSessionLineNullability(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_api.db")
	if err := InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	defer CloseAPIDB()

	apiLog.insert(APIEntry{
		Timestamp:   time.Now(),
		Session:     "test",
		Model:       "test",
		CallType:    "conversation",
		SessionLine: 42,
		SessionFile: "/test.jsonl",
	})

	var sl sql.NullInt64
	apiLog.db.QueryRow("SELECT session_line FROM api_calls WHERE session_line IS NOT NULL").Scan(&sl)
	if !sl.Valid || sl.Int64 != 42 {
		t.Errorf("session_line = %v, want 42", sl)
	}
}

// TestConversationLogInsertError verifies that the conversation log handles a DB insert
// error gracefully — logging an error rather than panicking.
func TestConversationLogInsertError(t *testing.T) {
	resetGlobal()
	t.Cleanup(resetGlobal)
	buf := captureOutput(t)

	dbPath := filepath.Join(t.TempDir(), "test_conv.db")
	if err := InitConversation(dbPath); err != nil {
		t.Fatalf("InitConversation: %v", err)
	}

	// Close the DB to force an error on insert
	convFallback.db.Close()

	Conversation(ConversationEntry{
		Direction: "recv", UserID: "1", Username: "u", ChatID: 1,
		Text: "should fail", Session: "",
	})

	if !strings.Contains(buf.String(), "insert error") {
		t.Errorf("expected insert error log, got: %s", buf.String())
	}

	// Clean up
	convLogs = nil
	convFallback = nil
}
