package memory

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"foci/internal/convo"
	"foci/internal/sqlite"
)

// ConversationMessage represents a single message from the conversation log.
type ConversationMessage struct {
	RowID   int64
	Time    time.Time
	Text    string
	Session string
	// Kind is convo.RowKind for the row: "" for ordinary conversation, else
	// the non-delivered turn kind and/or "subagent" (#2060).
	Kind string
}

// ConversationRef names one conversation row: the session key it was logged
// under and its row ID in that agent's conversation DB.
type ConversationRef struct {
	Session string
	RowID   int64
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

// threadContextQuery selects half rows before the target plus (lines-half)
// from the target onward, within one session and one thread (COALESCE'd
// subagent_group: "" is the main thread). Total = lines.
const threadContextQuery = `
	SELECT * FROM (
		SELECT id, ts, text, content_type, turn_kind FROM messages
		WHERE session = ? AND id < ? AND COALESCE(subagent_group, '') = ? ORDER BY id DESC LIMIT ?
	)
	UNION ALL
	SELECT * FROM (
		SELECT id, ts, text, content_type, turn_kind FROM messages
		WHERE session = ? AND id >= ? AND COALESCE(subagent_group, '') = ? ORDER BY id ASC LIMIT ?
	)
	ORDER BY id`

// legacyContextQuery is threadContextQuery for a DB without the #2060
// columns: the same window, unscoped, with no kinds.
const legacyContextQuery = `
	SELECT * FROM (
		SELECT id, ts, text, '', NULL FROM messages WHERE session = ? AND id < ? ORDER BY id DESC LIMIT ?
	)
	UNION ALL
	SELECT * FROM (
		SELECT id, ts, text, '', NULL FROM messages WHERE session = ? AND id >= ? ORDER BY id ASC LIMIT ?
	)
	ORDER BY id`

// ReadContext retrieves messages surrounding a specific message in a conversation session.
// Returns up to lines messages centered on the message with the given rowID.
//
// The window is scoped to the target's thread (#2060): a subagent row's context
// is the other rows of the same subagent (same subagent_group), and a
// main-thread row's context excludes subagent rows, so the two never
// interleave. A DB that predates those columns gets the unscoped window.
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

	// Scope the window to the target row's thread: its subagent_group, where
	// main-thread rows have none. A query error here (rather than no row) means
	// the DB predates the #2060 columns: fall back to the unscoped window.
	var group sql.NullString
	err = db.QueryRow(`SELECT subagent_group FROM messages WHERE id = ?`, rowID).Scan(&group)
	var rows *sql.Rows
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		rows, err = db.Query(legacyContextQuery, session, rowID, half, session, rowID, lines-half)
	} else {
		rows, err = db.Query(threadContextQuery,
			session, rowID, group.String, half, session, rowID, group.String, lines-half)
	}
	if err != nil {
		return nil, fmt.Errorf("query context: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var msgs []ConversationMessage
	for rows.Next() {
		var m ConversationMessage
		var ts, contentType string
		var turnKind sql.NullString
		if err := rows.Scan(&m.RowID, &ts, &m.Text, &contentType, &turnKind); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			m.Time = t
		}
		m.Session = session
		m.Kind = convo.RowKind(contentType, turnKind.String)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// Kinds returns convo.RowKind for each ref whose row is not ordinary
// conversation, so search results can label subagent and non-delivered-turn
// hits. Best effort: a ref whose agent has no DB, a DB that predates the #2060
// columns, or any read error is simply absent from the map (it reads as
// ordinary conversation). One DB open per agent.
func (cr *ConversationReader) Kinds(refs []ConversationRef) map[ConversationRef]string {
	out := make(map[ConversationRef]string)
	if cr == nil {
		return out
	}
	byAgent := make(map[string][]ConversationRef)
	for _, r := range refs {
		if r.RowID > 0 {
			a := sessionAgent(r.Session)
			byAgent[a] = append(byAgent[a], r)
		}
	}
	for agentID, agentRefs := range byAgent {
		dbPath, ok := cr.dbPaths[agentID]
		if !ok {
			continue
		}
		cr.kindsFromDB(dbPath, agentRefs, out)
	}
	return out
}

func (cr *ConversationReader) kindsFromDB(dbPath string, refs []ConversationRef, out map[ConversationRef]string) {
	db, err := sqlite.Open(dbPath)
	if err != nil {
		return
	}
	defer func() { _ = db.Close() }()
	for _, r := range refs {
		var contentType string
		var turnKind sql.NullString
		if err := db.QueryRow(`SELECT content_type, turn_kind FROM messages WHERE id = ? AND session = ?`,
			r.RowID, r.Session).Scan(&contentType, &turnKind); err != nil {
			continue
		}
		if k := convo.RowKind(contentType, turnKind.String); k != "" {
			out[r] = k
		}
	}
}

// sessionAgent extracts the agent ID from a session key.
// Session keys use slash-separated format: "{agentID}/{type}{id}[/{child}]".
func sessionAgent(session string) string {
	if idx := strings.IndexByte(session, '/'); idx > 0 {
		return session[:idx]
	}
	return ""
}
