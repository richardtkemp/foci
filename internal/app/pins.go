package app

import (
	"foci/internal/session"
)

// chatMetaPins is the chat_metadata key holding a conversation's pinned-message
// set, alongside the existing "draft" and "last_read" keys. The value is a JSON
// array of message IDs — a set, stored sorted so an unchanged set round-trips to
// an identical string (encode/decode via the shared session chat-set codec).
//
// This is MESSAGE pin (#893's bubble bookmark, surfaced in the media view's
// Pinned tab). The roster's CONVERSATION pin is a separate scalar mirror under
// chatMetaConvPinned (convpin_sync.go, #2313).
const chatMetaPins = "pins"

// applyPin folds one message's new pinned state into a set, returning the new
// set. Idempotent: pinning an already-pinned message, or unpinning one that
// is not in the set, returns an equivalent set. Never returns nil.
func applyPin(ids []string, messageID string, pinned bool) []string {
	out := make([]string, 0, len(ids)+1)
	for _, id := range ids {
		if id != messageID {
			out = append(out, id)
		}
	}
	if pinned {
		out = append(out, messageID)
	}
	return session.NormaliseChatSet(out)
}
