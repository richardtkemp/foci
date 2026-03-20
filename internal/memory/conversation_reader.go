package memory

import (
	"fmt"
	"strings"
	"time"

	"foci/internal/sqlite"
)

// ConversationMessage represents a single message from the conversation log.
type ConversationMessage struct {
	RowID   int64
	Time    time.Time
	Text    string
	Session string
}

// ConversationReader reads messages from per-agent conversation log databases
// to provide context around search results.
type ConversationReader struct {
	dbPaths map[string]string // agentID → conversation.db path
}

// NewConversationReader creates a reader that can fetch conversation context.
// dbPaths maps agent IDs to their conversation.db file paths.
func NewConversationReader(dbPaths map[string]string) *ConversationReader {
	if len(dbPaths) == 0 {
		return nil
	}
	return &ConversationReader{dbPaths: dbPaths}
}

// ReadContext retrieves messages surrounding a specific message in a conversation session.
// Returns up to lines messages centered on the message with the given rowID.
func (cr *ConversationReader) ReadContext(session string, rowID int64, lines int) ([]ConversationMessage, error) {
	agentID := sessionAgent(session)
	dbPath, ok := cr.dbPaths[agentID]
	if !ok {
		return nil, fmt.Errorf("no conversation database for agent %q", agentID)
	}

	db, err := sqlite.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open conversation db: %w", err)
	}
	defer func() { _ = db.Close() }()

	half := lines / 2

	// Get half messages before target + (lines-half) from target onward,
	// all within the same session. Total = lines.
	rows, err := db.Query(`
		SELECT id, ts, text FROM (
			SELECT id, ts, text FROM messages WHERE session = ? AND id < ? ORDER BY id DESC LIMIT ?
		)
		UNION ALL
		SELECT id, ts, text FROM (
			SELECT id, ts, text FROM messages WHERE session = ? AND id >= ? ORDER BY id ASC LIMIT ?
		)
		ORDER BY id
	`, session, rowID, half, session, rowID, lines-half)
	if err != nil {
		return nil, fmt.Errorf("query context: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var msgs []ConversationMessage
	for rows.Next() {
		var m ConversationMessage
		var ts string
		if err := rows.Scan(&m.RowID, &ts, &m.Text); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			m.Time = t
		}
		m.Session = session
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// sessionAgent extracts the agent ID from a session key.
// Session keys use slash-separated format: "{agentID}/{typeID}/{versionTS}".
func sessionAgent(session string) string {
	if idx := strings.IndexByte(session, '/'); idx > 0 {
		return session[:idx]
	}
	return ""
}
