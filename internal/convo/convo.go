// Package convo persists platform conversation messages to per-agent SQLite
// databases and offers an optional indexing hook (used to feed the memory
// search index). It was extracted from internal/log so the logging package can
// stay a lightweight leaf rather than carrying a data store.
package convo

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"foci/internal/log"
	"foci/internal/sqlite"
	"foci/internal/timeutil"
)

var (
	conversationLog = log.NewComponentLogger("conversation")
)

// Entry is a single message in the conversation log.
type Entry struct {
	Direction   string // "recv" or "sent"
	UserID      string
	Username    string
	ChatID      int64
	Text        string
	ParseMode   string // for sent messages: "Markdown", "", etc.
	Session     string
	Error       string // non-empty if send failed
	ContentType string // "text" (default) or "thinking"
}

// agentLog writes platform messages to a SQLite database.
type agentLog struct {
	db *sql.DB
	mu sync.Mutex
}

var (
	convLogs     map[string]*agentLog // agentID → log
	convFallback *agentLog            // used when session can't be routed
)

// Hook is called for each logged conversation entry. Set by the gateway to
// index conversation text into the memory index. rowID is the SQLite row ID
// from the conversation log INSERT.
var Hook func(text, session string, rowID int64)

// openLog opens a single conversation log database.
func openLog(path string) (*agentLog, error) {
	db, err := sqlite.OpenInit(path, `CREATE TABLE IF NOT EXISTS messages (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		ts           TEXT    NOT NULL,
		direction    TEXT    NOT NULL,
		user_id      TEXT    NOT NULL,
		username     TEXT    NOT NULL,
		chat_id      INTEGER NOT NULL,
		text         TEXT    NOT NULL,
		parse_mode   TEXT,
		session      TEXT,
		error        TEXT,
		content_type TEXT    NOT NULL DEFAULT 'text'
	)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_ts_unix ON messages(unixepoch(ts))`,
	)
	if err != nil {
		return nil, err
	}
	// Migration for existing DBs.
	_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN content_type TEXT NOT NULL DEFAULT 'text'`)
	return &agentLog{db: db}, nil
}

// InitPerAgent opens per-agent conversation log databases. pathFn maps each
// agent ID to its database path.
func InitPerAgent(agentIDs []string, pathFn func(string) string) error {
	m := make(map[string]*agentLog, len(agentIDs))
	for _, id := range agentIDs {
		cl, err := openLog(pathFn(id))
		if err != nil {
			// Close already-opened logs on failure.
			for _, opened := range m {
				_ = opened.db.Close()
			}
			return fmt.Errorf("init conversation log for %s: %w", id, err)
		}
		m[id] = cl
	}
	convLogs = m
	// Use the first agent as fallback for entries without a routable session.
	if len(agentIDs) > 0 {
		convFallback = m[agentIDs[0]]
	}
	return nil
}

// Close closes all conversation log databases.
func Close() {
	for _, cl := range convLogs {
		_ = cl.db.Close()
	}
	convLogs = nil
	convFallback = nil
}

// Record logs a conversation entry. No-op if not initialized.
func Record(entry Entry) {
	cl := resolveLog(entry.Session)
	if cl == nil {
		return
	}
	rowID := cl.insert(entry)

	if Hook != nil && entry.Text != "" {
		Hook(entry.Text, entry.Session, rowID)
	}
}

// resolveLog picks the per-agent log for a session key, falling back to the
// default log when the session can't be routed.
func resolveLog(session string) *agentLog {
	if len(convLogs) == 0 {
		return nil
	}
	if agentID := agentFromSession(session); agentID != "" {
		if cl, ok := convLogs[agentID]; ok {
			return cl
		}
	}
	return convFallback
}

// agentFromSession extracts the agent ID from a session key. Session keys use
// slash-separated format: "{agentID}/{type}{id}[/{child}]". Returns "" if the
// format doesn't match.
func agentFromSession(session string) string {
	if idx := strings.IndexByte(session, '/'); idx > 0 {
		return session[:idx]
	}
	return ""
}

// insert writes a conversation entry and returns the SQLite row ID (0 on error).
func (c *agentLog) insert(entry Entry) int64 {
	ts := timeutil.FormatNano(timeutil.Now())

	contentType := entry.ContentType
	if contentType == "" {
		contentType = "text"
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	res, err := c.db.Exec(
		`INSERT INTO messages (ts, direction, user_id, username, chat_id, text, parse_mode, session, error, content_type)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ts, entry.Direction, entry.UserID, entry.Username, entry.ChatID,
		entry.Text, entry.ParseMode, entry.Session, entry.Error, contentType,
	)
	if err != nil {
		conversationLog.Errorf("insert error: %v", err)
		return 0
	}
	rowID, _ := res.LastInsertId()
	return rowID
}
