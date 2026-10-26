package app

import (
	"errors"

	"foci/internal/fap"
)

// Agent-initiated close (#2573). The agent's close_session tool closes the app
// chat it is running in. "Closed" is the same state the user's own Close chat
// produces: the conversation leaves the shared open-set (system_state
// open_chats) and every device reconciles its tabs from a conversation.openSync.
// Dick's ruling (2026-10-26 15:09): nothing more — the conversation, its frames
// and its roster row stay, and the user's next message reopens it exactly as
// after a manual close. This is the fourth open-set writer, beside
// handleConversationOpenSet's replace, mintFacetConversation's append and the
// auto-close sweep's filter, and like them it runs its load→filter→store→fan-out
// under openSetMu.

// CloseSessionConversations closes every open app conversation bound to
// sessionKey (agent agentID) via the active hub, as if the user had closed it
// on a device. It returns how many conversations left the open-set; 0 means
// none was open (already closed, or the session has no app chat). Errors only
// when the app provider is not running.
func CloseSessionConversations(agentID, sessionKey string) (int, error) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return 0, errors.New("app provider not running")
	}
	return h.closeSessionConversations(agentID, sessionKey), nil
}

// closeSessionConversations removes from the shared open-set every conversation
// whose durable binding belongs to agentID and is bound to sessionKey — the
// same conv→session resolution OpenSessionsForAgent uses, so the chat that
// keepalive warms for this session is exactly the one closed. Survivors keep
// their order. A round that removes nothing writes and sends nothing. The
// broadcast has no sender to exclude (the close is server-originated, as in the
// sweep), so every connected device — including one showing the chat — closes
// its tab.
func (h *Hub) closeSessionConversations(agentID, sessionKey string) int {
	if sessionKey == "" {
		return 0
	}
	h.openSetMu.Lock()
	defer h.openSetMu.Unlock()
	ids := h.loadOpenChats()
	kept := make([]string, 0, len(ids)) // non-nil: an emptied set stores and sends [] rather than null
	for _, id := range ids {
		h.mu.RLock()
		b := h.convs[id]
		h.mu.RUnlock()
		if b != nil {
			b.mu.Lock()
			sk, ag := b.sessionKey, b.agentID
			b.mu.Unlock()
			if ag == agentID && sk == sessionKey {
				continue
			}
		}
		kept = append(kept, id)
	}
	closed := len(ids) - len(kept)
	if closed == 0 {
		return 0
	}
	h.storeOpenChats(kept)
	h.broadcastExcept(nil, fap.ConversationOpenSync{ConversationIDs: kept})
	appLog.Infof("agent close: agent=%s session=%s closed %d open chat(s) (%d remain)", agentID, sessionKey, closed, len(kept))
	return closed
}
