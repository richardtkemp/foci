package session

import (
	"encoding/json"
	"fmt"
)

// SystemStateOpenChats is the system_state key that holds the app's shared
// open set: a JSON array of the conversation ids of the chats the user has
// open, shared across devices (#1243). The app hub is its only writer.
const SystemStateOpenChats = "open_chats"

// OpenChatSessionKeys returns the session keys of every chat in the app's
// shared open set, across all agents.
//
// It resolves each conversation id from the persisted rows alone, the same
// way the hub's ensureBinding does: the conversation's 'conv_id'
// chat_metadata row gives its agent and chat id, and a 'session_key' row on
// that chat overrides the derived chat key (an adopted session or a depth
// leaf). So it gives the right answer before the app hub has rebuilt its
// bindings, as the archive sweep's startup run needs (#3134). A conversation
// with no 'conv_id' row has no binding and is skipped, as the hub skips it.
//
// An unset open set is an empty map. An unreadable one is an error: the
// callers hold transcripts on it, so they must not read "nothing open".
func (idx *SessionIndex) OpenChatSessionKeys() (map[string]bool, error) {
	raw, err := idx.GetSystemState(SystemStateOpenChats)
	if err != nil {
		return nil, fmt.Errorf("read open set: %w", err)
	}
	out := map[string]bool{}
	if raw == "" {
		return out, nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, fmt.Errorf("parse open set: %w", err)
	}
	if len(ids) == 0 {
		return out, nil
	}
	open := make(map[string]bool, len(ids))
	for _, id := range ids {
		open[id] = true
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()
	rows, err := idx.db.Query(
		`SELECT c.agent_id, c.chat_id, c.value, COALESCE(s.value, '')
		 FROM chat_metadata c
		 LEFT JOIN chat_metadata s
		   ON s.agent_id = c.agent_id AND s.platform = c.platform
		  AND s.chat_id = c.chat_id AND s.key = 'session_key'
		 WHERE c.platform = 'app' AND c.key = 'conv_id'
		   AND c.agent_id != '' AND c.value != ''`,
	)
	if err != nil {
		return nil, fmt.Errorf("resolve open set: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var (
			agentID, convID, sessionKey string
			chatID                      int64
		)
		if err := rows.Scan(&agentID, &chatID, &convID, &sessionKey); err != nil {
			return nil, fmt.Errorf("resolve open set: %w", err)
		}
		if !open[convID] {
			continue
		}
		if sessionKey == "" {
			sessionKey = NewChatSessionKey(agentID, chatID)
		}
		out[sessionKey] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve open set: %w", err)
	}
	return out, nil
}
