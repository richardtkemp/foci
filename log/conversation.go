package log

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
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

var convLog *ConversationLog

// ConversationHook is called for each logged conversation entry.
// Set by main.go to index conversation text into the memory FTS5 index.
var ConversationHook func(text, session string)

// InitConversation opens (or creates) the SQLite conversation log.
func InitConversation(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("open conversation db: %w", err)
	}

	// WAL mode for concurrent reads
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return fmt.Errorf("set WAL mode: %w", err)
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS messages (
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
		db.Close()
		return fmt.Errorf("create messages table: %w", err)
	}

	convLog = &ConversationLog{db: db}
	return nil
}

// CloseConversation closes the conversation log database.
func CloseConversation() {
	if convLog != nil {
		convLog.db.Close()
		convLog = nil
	}
}

// Conversation logs a conversation entry. No-op if not initialized.
func Conversation(entry ConversationEntry) {
	if convLog == nil {
		return
	}
	convLog.log(entry)

	if ConversationHook != nil && entry.Text != "" {
		ConversationHook(entry.Text, entry.Session)
	}
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
