package log

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"foci/internal/sqlite"
)

// ConversationEntry is a single message in the conversation log.
type ConversationEntry struct {
	Direction string // "recv" or "sent"
	UserID    string
	Username  string
	ChatID    int64
	Text      string
	ParseMode string // for sent messages: "Markdown", "", etc.
	Session   string
	Error     string // non-empty if send failed
}

// ConversationLog writes Telegram messages to a SQLite database.
type ConversationLog struct {
	db *sql.DB
	mu sync.Mutex
}

var (
	convLogs     map[string]*ConversationLog // agentID → log
	convFallback *ConversationLog            // used when session can't be routed
)

// ConversationHook is called for each logged conversation entry.
// Set by main.go to index conversation text into the memory FTS5 index.
var ConversationHook func(text, session string)

// openConversationLog opens a single conversation log database.
func openConversationLog(path string) (*ConversationLog, error) {
	db, err := sqlite.OpenInit(path, `CREATE TABLE IF NOT EXISTS messages (
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
		return nil, err
	}
	return &ConversationLog{db: db}, nil
}

// InitConversation opens a single conversation log (used by tests and single-agent setups).
func InitConversation(path string) error {
	cl, err := openConversationLog(path)
	if err != nil {
		return err
	}
	convLogs = map[string]*ConversationLog{"": cl}
	convFallback = cl
	return nil
}

// InitPerAgentConversation opens per-agent conversation log databases.
// pathFn maps each agent ID to its database path.
func InitPerAgentConversation(agentIDs []string, pathFn func(string) string) error {
	m := make(map[string]*ConversationLog, len(agentIDs))
	for _, id := range agentIDs {
		cl, err := openConversationLog(pathFn(id))
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

// CloseConversation closes all conversation log databases.
func CloseConversation() {
	for _, cl := range convLogs {
		_ = cl.db.Close()
	}
	convLogs = nil
	convFallback = nil
}

// Conversation logs a conversation entry. No-op if not initialized.
func Conversation(entry ConversationEntry) {
	cl := resolveConvLog(entry.Session)
	if cl == nil {
		return
	}
	cl.log(entry)

	if ConversationHook != nil && entry.Text != "" {
		ConversationHook(entry.Text, entry.Session)
	}
}

// resolveConvLog picks the per-agent log for a session key, falling back
// to the default log when the session can't be routed.
func resolveConvLog(session string) *ConversationLog {
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

// agentFromSession extracts the agent ID from a session key of the form
// "agent:<id>:...". Returns "" if the format doesn't match.
func agentFromSession(session string) string {
	if !strings.HasPrefix(session, "agent:") {
		return ""
	}
	rest := session[len("agent:"):]
	if idx := strings.IndexByte(rest, ':'); idx > 0 {
		return rest[:idx]
	}
	return ""
}

func (c *ConversationLog) log(entry ConversationEntry) {
	ts := time.Now().UTC().Format(time.RFC3339Nano)

	c.mu.Lock()
	defer c.mu.Unlock()

	_, err := c.db.Exec(
		`INSERT INTO messages (ts, direction, user_id, username, chat_id, text, parse_mode, session, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ts, entry.Direction, entry.UserID, entry.Username, entry.ChatID,
		entry.Text, entry.ParseMode, entry.Session, entry.Error,
	)
	if err != nil {
		std.event(ERROR, "conversation", "insert error: %v", err)
	}
}
