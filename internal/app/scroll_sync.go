package app

import (
	"encoding/json"

	"foci/internal/fap"
)

// chatMetaScroll is the chat_metadata key holding a conversation's last-left
// scroll position (#2144), alongside "draft", "last_read" and "pins". The value
// is the JSON form of storedScroll; an empty string means never written.
const chatMetaScroll = "scroll"

// storedScroll is the persisted form of a ScrollPut. Kept separate from the
// wire structs so the stored shape does not silently follow a wire rename.
type storedScroll struct {
	MessageID string `json:"messageId,omitempty"`
	Following bool   `json:"following"`
}

// normaliseScroll reports whether a put carries a usable position and returns
// its stored form. Following needs no message (the receiver lands at the newest
// one), so any id sent with it is dropped rather than stored as noise; a
// not-following position without a message names nowhere and is refused.
func normaliseScroll(messageID string, following bool) (storedScroll, bool) {
	if following {
		return storedScroll{Following: true}, true
	}
	if messageID == "" {
		return storedScroll{}, false
	}
	return storedScroll{MessageID: messageID}, true
}

func encodeScroll(s storedScroll) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// decodeScroll parses a stored position. ok is false for an empty or
// unreadable value, which the replay skips rather than sending as a position.
func decodeScroll(v string) (storedScroll, bool) {
	if v == "" {
		return storedScroll{}, false
	}
	var s storedScroll
	if err := json.Unmarshal([]byte(v), &s); err != nil {
		return storedScroll{}, false
	}
	return normaliseScroll(s.MessageID, s.Following)
}

// handleScroll persists where the user left a conversation and mirrors it to
// the user's other devices — the draft mirror's shape exactly (#2144): a dumb
// last-write-wins store plus a fan-out that skips the sender. Conflict
// resolution between two devices moving the same chat lives on the clients
// (ScrollSyncGate), as it does for drafts. Fire-and-forget: ScrollPut is not
// conversation-reliability-scoped (see inboundConvID), so there is no ack.
func (h *Hub) handleScroll(client *wsClient, f fap.ScrollPut) {
	s, ok := normaliseScroll(f.MessageID, f.Following)
	if !ok {
		return
	}
	h.mu.RLock()
	b := h.convs[f.ConversationID]
	h.mu.RUnlock()
	if b == nil {
		return
	}
	if idx := h.deps.SessionIndex; idx != nil {
		if err := idx.SetChatMetadata(b.agentID, "app", b.chatID, chatMetaScroll, encodeScroll(s)); err != nil {
			appLog.Warnf("scroll %s: persist: %v", f.ConversationID, err)
		}
	}
	h.broadcastExcept(client, fap.ScrollSync{ConversationID: f.ConversationID, MessageID: s.MessageID, Following: s.Following})
}

// pushScrolls replays the stored scroll position of every live conversation to
// a just-connected client, so a device that was offline when the user moved on
// another device still opens the chat there. replayEmpty is false: a chat that
// never had a position written has nothing to say, and replaying it as one
// would override every device's own local restore on the first hello.
func (h *Hub) pushScrolls(client *wsClient) {
	h.pushChatScalar(client, chatMetaScroll, false, func(convID, v string) fap.ServerFrame {
		s, ok := decodeScroll(v)
		if !ok {
			return nil
		}
		return fap.ScrollSync{ConversationID: convID, MessageID: s.MessageID, Following: s.Following}
	})
}
