package memory

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/sqlite"
)

func seedConversationDB(t *testing.T, dir, session string, texts []string) string {
	t.Helper()
	dbPath := filepath.Join(dir, "conversation.db")
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

	base := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	for i, text := range texts {
		ts := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano)
		_, err := db.Exec(
			`INSERT INTO messages (ts, direction, user_id, username, chat_id, text, parse_mode, session, error)
			 VALUES (?, 'recv', 'u1', 'user', 100, ?, '', ?, '')`,
			ts, text, session,
		)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	return dbPath
}

func TestConversationReaderReadContext(t *testing.T) {
	// Verifies that ReadContext returns the correct window of messages
	// centered on the target rowID within the same session.
	t.Parallel()
	dir := t.TempDir()
	session := "agent1/c100/1000"
	texts := []string{
		"msg one",
		"msg two",
		"msg three",
		"msg four",
		"msg five",
		"msg six",
		"msg seven",
	}
	dbPath := seedConversationDB(t, dir, session, texts)

	cr := NewConversationReader(map[string]string{"agent1": dbPath})
	if cr == nil {
		t.Fatal("expected non-nil ConversationReader")
	}

	// Fetch 4 messages around row 4 (half=2 before, 1 target + 1 after)
	msgs, err := cr.ReadContext(session, 4, 4)
	if err != nil {
		t.Fatalf("ReadContext: %v", err)
	}

	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}

	// Should be rows 2,3,4,5
	if msgs[0].RowID != 2 || msgs[1].RowID != 3 || msgs[2].RowID != 4 || msgs[3].RowID != 5 {
		ids := make([]int64, len(msgs))
		for i, m := range msgs {
			ids[i] = m.RowID
		}
		t.Errorf("unexpected row IDs: %v", ids)
	}

	// Check text content
	if msgs[2].Text != "msg four" {
		t.Errorf("target message text = %q, want %q", msgs[2].Text, "msg four")
	}

	// Check timestamps are populated
	if msgs[0].Time.IsZero() {
		t.Error("expected non-zero timestamp on messages")
	}
}

func TestConversationReaderEdgeCases(t *testing.T) {
	// Verifies ReadContext handles edge cases: target at beginning/end of session,
	// and requesting more lines than available.
	t.Parallel()
	dir := t.TempDir()
	session := "agent1/c100/1000"
	texts := []string{"alpha", "beta", "gamma"}
	dbPath := seedConversationDB(t, dir, session, texts)

	cr := NewConversationReader(map[string]string{"agent1": dbPath})

	// Target at beginning — row 1, lines=6 (more than exist)
	msgs, err := cr.ReadContext(session, 1, 6)
	if err != nil {
		t.Fatalf("ReadContext at start: %v", err)
	}
	if len(msgs) != 3 {
		t.Errorf("expected all 3 messages, got %d", len(msgs))
	}

	// Target at end — row 3
	msgs, err = cr.ReadContext(session, 3, 4)
	if err != nil {
		t.Fatalf("ReadContext at end: %v", err)
	}
	if len(msgs) < 2 {
		t.Errorf("expected at least 2 messages, got %d", len(msgs))
	}
}

func TestConversationReaderIsolatesSession(t *testing.T) {
	// Verifies that ReadContext only returns messages from the specified session,
	// not from other sessions that share the same conversation DB.
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "conversation.db")

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
		t.Fatalf("create db: %v", err)
	}

	// Insert interleaved messages from two sessions
	base := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	sessions := []string{"agent1/c100/1000", "agent1/c200/2000"}
	for i := 0; i < 6; i++ {
		sess := sessions[i%2]
		ts := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano)
		_, _ = db.Exec(
			`INSERT INTO messages (ts, direction, user_id, username, chat_id, text, parse_mode, session, error)
			 VALUES (?, 'recv', 'u1', 'user', 100, ?, '', ?, '')`,
			ts, "msg-"+sess, sess,
		)
	}
	_ = db.Close()

	cr := NewConversationReader(map[string]string{"agent1": dbPath})

	// Should only get messages from session c100
	msgs, err := cr.ReadContext("agent1/c100/1000", 1, 10)
	if err != nil {
		t.Fatalf("ReadContext: %v", err)
	}
	for _, m := range msgs {
		if m.Session != "agent1/c100/1000" {
			t.Errorf("got message from wrong session: %q", m.Session)
		}
	}
}

func TestNewConversationReaderNil(t *testing.T) {
	// Verifies that NewConversationReader returns nil when given empty paths.
	t.Parallel()
	cr := NewConversationReader(nil)
	if cr != nil {
		t.Error("expected nil for empty dbPaths")
	}
	cr = NewConversationReader(map[string]string{})
	if cr != nil {
		t.Error("expected nil for empty map")
	}
}

func TestConversationReaderUnknownAgent(t *testing.T) {
	// Verifies that ReadContext returns an error for an unknown agent.
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "conversation.db")
	os.WriteFile(dbPath, nil, 0644) // dummy file
	cr := NewConversationReader(map[string]string{"known": dbPath})
	_, err := cr.ReadContext("unknown/c100/1000", 1, 10)
	if err == nil {
		t.Error("expected error for unknown agent")
	}
}
