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

// MarkSessionRead marks the app chat bound to sessionKey (agent agentID) read
// on every device, as if the user had read it (#2592, close_session's
// mark_read). With a turn in flight on that chat the read waits for the turn's
// end, so it covers the agent's final reply; otherwise it applies now. It
// reports how many chats were marked or scheduled (0 or 1: a session binds one
// chat). Errors only when the app provider is not running.
func MarkSessionRead(agentID, sessionKey string) (int, error) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return 0, errors.New("app provider not running")
	}
	return h.markSessionRead(agentID, sessionKey), nil
}

// markSessionRead resolves the session's binding (bySession, the binding its
// turns render to) and either flags it for finishTurnRead or reads it now.
// "In flight" is the turn-scoped activity appSink drives: non-idle from
// TurnStart until TurnComplete or the turn's cleanup.
func (h *Hub) markSessionRead(agentID, sessionKey string) int {
	if sessionKey == "" {
		return 0
	}
	b := h.bindingForSession(sessionKey)
	if b == nil {
		return 0
	}
	b.mu.Lock()
	if b.agentID != agentID {
		b.mu.Unlock()
		return 0
	}
	inTurn := b.turnKind != "" && b.turnKind != fap.ActivityKindIdle
	if inTurn {
		b.readAfterTurn = true
	}
	b.mu.Unlock()
	if !inTurn {
		h.readThroughLatest(b)
	}
	appLog.Infof("agent mark read: agent=%s session=%s conv=%s deferred=%v", agentID, sessionKey, b.convID, inTurn)
	return 1
}

// finishTurnRead applies a mark_read deferred to this turn's end, once the
// turn's final frames are sent. Called from appSink's cleanup; a no-op when
// none is pending.
func (h *Hub) finishTurnRead(b *convBinding) {
	b.mu.Lock()
	pending := b.readAfterTurn
	b.readAfterTurn = false
	b.mu.Unlock()
	if pending {
		h.readThroughLatest(b)
	}
}

// readThroughLatest moves b's read watermark to the newest message sent on it
// and mirrors it to every device. Nothing is sent when no message is known
// (none since the server started).
func (h *Hub) readThroughLatest(b *convBinding) {
	b.mu.Lock()
	id := b.lastMsgID
	b.mu.Unlock()
	if id == "" {
		appLog.Infof("agent mark read: conv=%s has no message to mark read", b.convID)
		return
	}
	h.applyRead(nil, b, id)
}

// SessionChatClosed reports whether the app chat bound to sessionKey (agent
// agentID) is closed: known is true when the session has an app chat at all,
// closed when none of its conversations is in the shared open-set (#2684: a
// spawn whose spawner's chat is closed has nobody to ask). A session with no
// app chat (another platform's, or a branch with no conversation) is not
// known; neither is anything while the app provider is not running.
func SessionChatClosed(agentID, sessionKey string) (closed, known bool) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil || sessionKey == "" {
		return false, false
	}
	return h.sessionChatClosed(agentID, sessionKey)
}

func (h *Hub) sessionChatClosed(agentID, sessionKey string) (closed, known bool) {
	bound := map[string]bool{}
	h.mu.RLock()
	convs := make(map[string]*convBinding, len(h.convs))
	for id, b := range h.convs {
		convs[id] = b
	}
	h.mu.RUnlock()
	for id, b := range convs {
		if b == nil {
			continue
		}
		b.mu.Lock()
		sk, ag := b.sessionKey, b.agentID
		b.mu.Unlock()
		if ag == agentID && sk == sessionKey {
			bound[id] = true
		}
	}
	if len(bound) == 0 {
		return false, false
	}
	for _, id := range h.loadOpenChats() {
		if bound[id] {
			return false, true
		}
	}
	return true, true
}
