package app

import (
	"foci/internal/fap"
)

// chatMetaConvPinned is the chat_metadata key holding a conversation's roster
// pin (#2313), alongside "draft", "last_read", "pins" and "scroll". The value
// is "1" for pinned, "0" for explicitly unpinned; an empty string means never
// written. This is the CONVERSATION pin (the roster's sort-to-top flag); the
// pinned MESSAGE set is chatMetaPins (pins.go).
const chatMetaConvPinned = "conv_pinned"

// encodeConvPin renders a pin state for storage. The inverse of decodeConvPin.
func encodeConvPin(pinned bool) string {
	if pinned {
		return "1"
	}
	return "0"
}

// decodeConvPin parses a stored pin state. known is false for an empty or
// unrecognised value: no writer of ours can produce one, so treating it as
// "the server cannot say" rather than "unpinned" can never wipe a device's
// local-only pin off a hand-corrupted row.
func decodeConvPin(v string) (pinned, known bool) {
	switch v {
	case "1":
		return true, true
	case "0":
		return false, true
	}
	return false, false
}

// handleConvPin persists the roster's CONVERSATION pin and mirrors it to the
// user's other devices — the scroll mirror's shape exactly (#2313): a scalar
// last-write-wins store plus a fan-out that skips the sender. Not a
// read-modify-write, so there is no lock (contrast handlePin, whose pinsMu
// exists only because pin sets fold). Fire-and-forget: ConvPinPut is not
// conversation-reliability-scoped (see inboundConvID), so there is no ack.
func (h *Hub) handleConvPin(client *wsClient, f fap.ConvPinPut) {
	h.mu.RLock()
	b := h.convs[f.ConversationID]
	h.mu.RUnlock()
	if b == nil {
		return
	}
	if idx := h.deps.SessionIndex; idx != nil {
		if err := idx.SetChatMetadata(b.agentID, "app", b.chatID, chatMetaConvPinned, encodeConvPin(f.Pinned)); err != nil {
			appLog.Warnf("convpin %s: persist: %v", f.ConversationID, err)
		}
	}
	h.broadcastExcept(client, fap.ConvPinSync{ConversationID: f.ConversationID, Pinned: f.Pinned})
}

// pushConvPins replays the stored conversation pin of every live conversation
// to a just-connected client, so a device that was offline when the user
// pinned or unpinned on another device catches up (#2313).
//
// replayEmpty is FALSE, for the same reason as pushPins. An unpinned
// conversation stores "0", which is a NON-empty string and so replays either
// way — the unpin still reaches a device that was offline for it. A truly
// empty stored value means something different: this conversation's pin has
// never been written by any device since the mirror shipped.
//
// Replaying THAT as "unpinned" would be a data-loss bug at upgrade. The
// conversation pin was a per-device preference until this mirror (#1882), and
// no client build uploads its local pins yet, so every device holds pins the
// server has never heard of. Sending "not pinned" for every never-written
// conversation on the first post-upgrade hello would clear them. Skipping it
// lets the local pins stand until the user's next pin or unpin in that
// conversation writes the key and makes the server's value authoritative.
func (h *Hub) pushConvPins(client *wsClient) {
	h.pushChatScalar(client, chatMetaConvPinned, false, func(convID, v string) fap.ServerFrame {
		pinned, known := decodeConvPin(v)
		if !known {
			return nil
		}
		return fap.ConvPinSync{ConversationID: convID, Pinned: pinned}
	})
}
