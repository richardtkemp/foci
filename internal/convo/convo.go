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
	"foci/internal/session"
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
	ContentType string // "text" (default), "thinking", or a subagent content type (ContentTypeSubagent*)

	// SubagentGroup and SubagentRun identify the subagent a ContentTypeSubagent*
	// row belongs to: the group is the Agent tool_use id (stable across
	// SendMessage reactivations; it also names the backend transcript), the run
	// is which run of that group. Empty/0 on main-thread rows.
	SubagentGroup string
	SubagentRun   int

	// TurnKind names the kind of NON-DELIVERED turn that produced the row — the
	// turn trigger (reflection, session_end_memory, background, consolidation,
	// branch). Empty for ordinary delivered conversation.
	TurnKind string
}

// Content types for subagent rows. Both are agent-authored, so they are
// recorded with Direction "sent".
const (
	ContentTypeSubagent       = "subagent"        // a subagent text block
	ContentTypeSubagentPrompt = "subagent_prompt" // the instruction a subagent run was given (start prompt or SendMessage follow-up)
)

// IsSubagentContentType reports whether ct is one of the subagent content types.
func IsSubagentContentType(ct string) bool {
	return ct == ContentTypeSubagent || ct == ContentTypeSubagentPrompt
}

// RowKind is the search-facing label for a messages row: "" for ordinary
// conversation, otherwise the turn kind and/or "subagent", joined by "/" (e.g.
// "reflection", "subagent", "reflection/subagent"). memory_search shows it so a
// hit can say it came from a subagent or a non-delivered turn rather than the
// chat.
func RowKind(contentType, turnKind string) string {
	var parts []string
	if turnKind != "" {
		parts = append(parts, turnKind)
	}
	if IsSubagentContentType(contentType) {
		parts = append(parts, "subagent")
	}
	return strings.Join(parts, "/")
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
		content_type TEXT    NOT NULL DEFAULT 'text',
		subagent_group TEXT,
		subagent_run   INTEGER,
		turn_kind      TEXT
	)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_ts_unix ON messages(unixepoch(ts))`,
	)
	if err != nil {
		return nil, err
	}
	// Migrations for existing DBs. Each ALTER fails harmlessly ("duplicate
	// column") once the column exists.
	_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN content_type TEXT NOT NULL DEFAULT 'text'`)
	_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN subagent_group TEXT`)
	_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN subagent_run INTEGER`)
	_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN turn_kind TEXT`)
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
func resolveLog(sessionKey string) *agentLog {
	if len(convLogs) == 0 {
		return nil
	}
	if agentID := session.AgentIDFromKey(sessionKey); agentID != "" {
		if cl, ok := convLogs[agentID]; ok {
			return cl
		}
	}
	return convFallback
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
		`INSERT INTO messages (ts, direction, user_id, username, chat_id, text, parse_mode, session, error, content_type,
		                       subagent_group, subagent_run, turn_kind)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ts, entry.Direction, entry.UserID, entry.Username, entry.ChatID,
		entry.Text, entry.ParseMode, entry.Session, entry.Error, contentType,
		nullIfEmpty(entry.SubagentGroup), nullIfZero(entry.SubagentRun), nullIfEmpty(entry.TurnKind),
	)
	if err != nil {
		conversationLog.Errorf("insert error: %v", err)
		return 0
	}
	rowID, _ := res.LastInsertId()
	return rowID
}

// nullIfEmpty stores an unset optional TEXT column as NULL, so main-thread rows
// read back exactly as they did before the column existed.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullIfZero is nullIfEmpty for an optional INTEGER column.
func nullIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}
